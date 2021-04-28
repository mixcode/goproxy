package goproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type ConnectActionLiteral int

const (
	ConnectAccept = iota
	ConnectReject
	ConnectMitm
	ConnectHijack
	ConnectHTTPMitm
	ConnectProxyAuthHijack
)

var (
	OkConnect       = &ConnectAction{Action: ConnectAccept, TLSConfig: TLSConfigFromCA(&GoproxyCa)}
	MitmConnect     = &ConnectAction{Action: ConnectMitm, TLSConfig: TLSConfigFromCA(&GoproxyCa)}
	HTTPMitmConnect = &ConnectAction{Action: ConnectHTTPMitm, TLSConfig: TLSConfigFromCA(&GoproxyCa)}
	RejectConnect   = &ConnectAction{Action: ConnectReject, TLSConfig: TLSConfigFromCA(&GoproxyCa)}
	httpsRegexp     = regexp.MustCompile(`^https:\/\/`)
)

// ConnectAction enables the caller to override the standard connect flow.
// When Action is ConnectHijack, it is up to the implementer to send the
// HTTP 200, or any other valid http response back to the client from within the
// Hijack func
type ConnectAction struct {
	Action    ConnectActionLiteral
	Hijack    func(req *http.Request, client net.Conn, ctx *ProxyCtx)
	TLSConfig func(host string, ctx *ProxyCtx) (*tls.Config, error)
}

func stripPort(s string) string {
	port := hasPort.FindString(s) // Find ':\d+$' part
	return s[:len(s)-len(port)]
}

func (proxy *ProxyHttpServer) dial(network, addr string) (c net.Conn, err error) {
	if proxy.Tr.DialContext != nil {
		return proxy.Tr.DialContext(context.Background(), network, addr)
	}
	return net.Dial(network, addr)
}

func (proxy *ProxyHttpServer) connectDial(network, addr string) (c net.Conn, err error) {
	if proxy.ConnectDial == nil {
		return proxy.dial(network, addr)
	}
	return proxy.ConnectDial(network, addr)
}

type halfClosable interface {
	net.Conn
	CloseWrite() error
	CloseRead() error
}

var _ halfClosable = (*net.TCPConn)(nil)

func (proxy *ProxyHttpServer) handleHttps(w http.ResponseWriter, r *http.Request) {
	proxyCtx := &ProxyCtx{Req: r, Session: atomic.AddInt64(&proxy.sess, 1), Proxy: proxy, certStore: proxy.CertStore}

	// Hijack open client connection to directly stream data
	hij, ok := w.(http.Hijacker)
	if !ok {
		panic("httpserver does not support hijacking")
	}
	proxyResponseWriter, _, e := hij.Hijack()
	if e != nil {
		panic("Cannot hijack connection " + e.Error())
	}

	// Find an appreciate connect handler
	proxyCtx.Logf("Running %d CONNECT handlers", len(proxy.httpsHandlers))
	todo, host := OkConnect, r.URL.Host
	for i, h := range proxy.httpsHandlers {
		newtodo, newhost := h.HandleHttpConnect(host, proxyCtx)

		if newtodo != nil {
			// If found a result, break the loop immediately
			proxyCtx.Logf("on %dth handler: %v %s", i, todo, host)
			todo, host = newtodo, newhost
			break
		}
	}

	switch todo.Action {

	case ConnectAccept:
		if !hasPort.MatchString(host) {
			host += ":80"
		}
		targetSiteCon, err := proxy.connectDial("tcp", host)
		if err != nil {
			httpError(proxyResponseWriter, proxyCtx, err)
			return
		}
		proxyCtx.Logf("Accepting CONNECT to %s", host)
		proxyResponseWriter.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))

		targetTCP, targetOK := targetSiteCon.(halfClosable)
		proxyClientTCP, clientOK := proxyResponseWriter.(halfClosable)
		if targetOK && clientOK {
			go copyAndClose(proxyCtx, targetTCP, proxyClientTCP)
			go copyAndClose(proxyCtx, proxyClientTCP, targetTCP)
		} else {
			go func() {
				var wg sync.WaitGroup
				wg.Add(2)
				go copyOrWarn(proxyCtx, targetSiteCon, proxyResponseWriter, &wg)
				go copyOrWarn(proxyCtx, proxyResponseWriter, targetSiteCon, &wg)
				wg.Wait()
				proxyResponseWriter.Close()
				targetSiteCon.Close()

			}()
		}

	case ConnectHijack:
		todo.Hijack(r, proxyResponseWriter, proxyCtx)

	case ConnectHTTPMitm:
		proxyResponseWriter.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
		proxyCtx.Logf("Assuming CONNECT is plain HTTP tunneling, mitm proxying it")
		targetSiteCon, err := proxy.connectDial("tcp", host)
		if err != nil {
			proxyCtx.Warnf("Error dialing to %s: %s", host, err.Error())
			return
		}
		for {
			client := bufio.NewReader(proxyResponseWriter)
			remote := bufio.NewReader(targetSiteCon)
			req, err := http.ReadRequest(client)
			if err != nil && err != io.EOF {
				proxyCtx.Warnf("cannot read request of MITM HTTP client: %+#v", err)
			}
			if err != nil {
				return
			}
			req, resp := proxy.filterRequest(req, proxyCtx)
			if resp == nil {
				if err := req.Write(targetSiteCon); err != nil {
					httpError(proxyResponseWriter, proxyCtx, err)
					return
				}
				resp, err = http.ReadResponse(remote, req)
				if err != nil {
					httpError(proxyResponseWriter, proxyCtx, err)
					return
				}
				defer resp.Body.Close()
			}
			resp = proxy.filterResponse(resp, proxyCtx)
			if err := resp.Write(proxyResponseWriter); err != nil {
				httpError(proxyResponseWriter, proxyCtx, err)
				return
			}
		}

	case ConnectMitm:
		proxyResponseWriter.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
		proxyCtx.Logf("Assuming CONNECT is TLS, mitm proxying it")
		// this goes in a separate goroutine, so that the net/http server won't think we're
		// still handling the request even after hijacking the connection. Those HTTP CONNECT
		// request can take forever, and the server will be stuck when "closed".
		// TODO: Allow Server.Close() mechanism to shut down this connection as nicely as possible
		tlsConfig := defaultTLSConfig
		if todo.TLSConfig != nil {
			var err error
			tlsConfig, err = todo.TLSConfig(host, proxyCtx)
			if err != nil {
				httpError(proxyResponseWriter, proxyCtx, err)
				return
			}
		}
		go func() {
			//TODO: cache connections to the remote website

			// Create a TLS server toward client
			rawClientTls := tls.Server(proxyResponseWriter, tlsConfig)
			if err := rawClientTls.Handshake(); err != nil {
				proxyCtx.Warnf("Cannot handshake client %v %v", r.Host, err)
				return
			}
			defer rawClientTls.Close()

			clientTlsReader := bufio.NewReader(rawClientTls)
			for !isEof(clientTlsReader) {
				req, err := http.ReadRequest(clientTlsReader)
				if err != nil && err != io.EOF {
					return
				}

				localProxyCtx := &ProxyCtx{Req: req, Session: atomic.AddInt64(&proxy.sess, 1), Proxy: proxy, UserData: proxyCtx.UserData}

				if err != nil {
					localProxyCtx.Warnf("Cannot read TLS request from mitm'd client %v %v", r.Host, err)
					return
				}
				req.RemoteAddr = r.RemoteAddr // since we're converting the request, need to carry over the original connecting IP as well
				localProxyCtx.Logf("req %v (%s)", r.Host, req.Host)

				if !httpsRegexp.MatchString(req.URL.String()) {
					req.URL, err = url.Parse("https://" + r.Host + req.URL.String())
				}

				// Bug fix which goproxy fails to provide request
				// information URL in the context when does HTTPS MITM
				localProxyCtx.Req = req

				// do pre-request filterings
				req, resp := proxy.filterRequest(req, localProxyCtx)

				// run the request
				if resp == nil {
					if isWebSocketRequest(req) {
						localProxyCtx.Logf("Request looks like websocket upgrade.")
						proxy.serveWebsocketTLS(localProxyCtx, w, req, tlsConfig, rawClientTls)
						return
					}
					if err != nil {
						localProxyCtx.Warnf("Illegal URL %s", "https://"+r.Host+req.URL.Path)
						return
					}
					removeProxyHeaders(localProxyCtx, req)
					resp, err = localProxyCtx.RoundTrip(req)
					if err != nil {
						localProxyCtx.Warnf("Cannot read TLS response from mitm'd server %v", err)
						return
					}
					localProxyCtx.Logf("resp %v", resp.Status)
				}

				// do post-request filterings
				resp = proxy.filterResponse(resp, localProxyCtx)

				// Write http response to client
				func() {
					defer resp.Body.Close()

					text := resp.Status
					statusCode := strconv.Itoa(resp.StatusCode) + " "
					text = strings.TrimPrefix(text, statusCode)
					// always use 1.1 to support chunked encoding
					if _, err = io.WriteString(rawClientTls, "HTTP/1.1"+" "+statusCode+text+"\r\n"); err != nil {
						localProxyCtx.Warnf("Cannot write TLS response HTTP status from mitm'd client: %v", err)
						return
					}
					// Since we don't know the length of resp, return chunked encoded response
					// TODO: use a more reasonable scheme
					resp.Header.Del("Content-Length")
					resp.Header.Set("Transfer-Encoding", "chunked")
					// Force connection close otherwise chrome will keep CONNECT tunnel open forever
					resp.Header.Set("Connection", "close")
					if err = resp.Header.Write(rawClientTls); err != nil {
						localProxyCtx.Warnf("Cannot write TLS response header from mitm'd client: %v", err)
						return
					}
					if _, err = io.WriteString(rawClientTls, "\r\n"); err != nil {
						localProxyCtx.Warnf("Cannot write TLS response header end from mitm'd client: %v", err)
						return
					}
					chunked := newChunkedWriter(rawClientTls)
					if _, err = io.Copy(chunked, resp.Body); e != nil {
						localProxyCtx.Warnf("Cannot write TLS response body from mitm'd client: %v", err)
						return
					}
					if err = chunked.Close(); e != nil {
						localProxyCtx.Warnf("Cannot write TLS chunked EOF from mitm'd client: %v", err)
						return
					}
					if _, err = io.WriteString(rawClientTls, "\r\n"); err != nil {
						localProxyCtx.Warnf("Cannot write TLS response chunked trailer from mitm'd client: %v", err)
						return
					}
				}() // resp.Body.close() runs here
				if err != nil {
					return
				}
			}
			proxyCtx.Logf("Exiting on EOF")
		}()

	case ConnectProxyAuthHijack:
		proxyResponseWriter.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n"))
		todo.Hijack(r, proxyResponseWriter, proxyCtx)

	case ConnectReject:
		if proxyCtx.Resp != nil {
			if err := proxyCtx.Resp.Write(proxyResponseWriter); err != nil {
				proxyCtx.Warnf("Cannot write response that reject http CONNECT: %v", err)
			}
		}
		proxyResponseWriter.Close()
	}
}

func httpError(w io.WriteCloser, ctx *ProxyCtx, err error) {
	if _, err := io.WriteString(w, "HTTP/1.1 502 Bad Gateway\r\n\r\n"); err != nil {
		ctx.Warnf("Error responding to client: %s", err)
	}
	if err := w.Close(); err != nil {
		ctx.Warnf("Error closing client connection: %s", err)
	}
}

func copyOrWarn(ctx *ProxyCtx, dst io.Writer, src io.Reader, wg *sync.WaitGroup) {
	if _, err := io.Copy(dst, src); err != nil {
		ctx.Warnf("Error copying to client: %s", err)
	}
	wg.Done()
}

func copyAndClose(ctx *ProxyCtx, dst, src halfClosable) {
	if _, err := io.Copy(dst, src); err != nil {
		ctx.Warnf("Error copying to client: %s", err)
	}

	dst.CloseWrite()
	src.CloseRead()
}

func dialerFromEnv(proxy *ProxyHttpServer) func(network, addr string) (net.Conn, error) {
	https_proxy := os.Getenv("HTTPS_PROXY")
	if https_proxy == "" {
		https_proxy = os.Getenv("https_proxy")
	}
	if https_proxy == "" {
		return nil
	}
	return proxy.NewConnectDialToProxy(https_proxy)
}

func (proxy *ProxyHttpServer) NewConnectDialToProxy(https_proxy string) func(network, addr string) (net.Conn, error) {
	return proxy.NewConnectDialToProxyWithHandler(https_proxy, nil)
}

func (proxy *ProxyHttpServer) NewConnectDialToProxyWithHandler(https_proxy string, connectReqHandler func(req *http.Request)) func(network, addr string) (net.Conn, error) {
	u, err := url.Parse(https_proxy)
	if err != nil {
		return nil
	}
	if u.Scheme == "" || u.Scheme == "http" {
		if !hasPort.MatchString(u.Host) {
			u.Host += ":80"
		}
		return func(network, addr string) (net.Conn, error) {
			connectReq := &http.Request{
				Method: "CONNECT",
				URL:    &url.URL{Opaque: addr},
				Host:   addr,
				Header: make(http.Header),
			}
			if connectReqHandler != nil {
				connectReqHandler(connectReq)
			}
			c, err := proxy.dial(network, u.Host)
			if err != nil {
				return nil, err
			}
			connectReq.Write(c)
			// Read response.
			// Okay to use and discard buffered reader here, because
			// TLS server will not speak until spoken to.
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, connectReq)
			if err != nil {
				c.Close()
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				resp, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					return nil, err
				}
				c.Close()
				return nil, errors.New("proxy refused connection" + string(resp))
			}
			return c, nil
		}
	}
	if u.Scheme == "https" || u.Scheme == "wss" {
		if !hasPort.MatchString(u.Host) {
			u.Host += ":443"
		}
		return func(network, addr string) (net.Conn, error) {
			c, err := proxy.dial(network, u.Host)
			if err != nil {
				return nil, err
			}
			c = tls.Client(c, proxy.Tr.TLSClientConfig)
			connectReq := &http.Request{
				Method: "CONNECT",
				URL:    &url.URL{Opaque: addr},
				Host:   addr,
				Header: make(http.Header),
			}
			if connectReqHandler != nil {
				connectReqHandler(connectReq)
			}
			connectReq.Write(c)
			// Read response.
			// Okay to use and discard buffered reader here, because
			// TLS server will not speak until spoken to.
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, connectReq)
			if err != nil {
				c.Close()
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				body, err := ioutil.ReadAll(io.LimitReader(resp.Body, 500))
				if err != nil {
					return nil, err
				}
				c.Close()
				return nil, errors.New("proxy refused connection" + string(body))
			}
			return c, nil
		}
	}
	return nil
}

func TLSConfigFromCA(ca *tls.Certificate) func(host string, ctx *ProxyCtx) (*tls.Config, error) {
	return func(host string, ctx *ProxyCtx) (*tls.Config, error) {
		serverName := stripPort(host)
		config := defaultTLSConfig.Clone()
		config.GetCertificate = func(hello *tls.ClientHelloInfo) (cert *tls.Certificate, err error) {

			if hello.ServerName != "" {
				serverName = hello.ServerName
			}

			ctx.Logf("signing for %s", serverName)

			genCert := func() (*tls.Certificate, error) {
				return signHost(*ca, []string{serverName})
			}
			if ctx.certStore != nil {
				cert, err = ctx.certStore.Fetch(serverName, genCert)
			} else {
				cert, err = genCert()
			}
			return
		}
		return config, nil
	}
}
