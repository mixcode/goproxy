module github.com/mixcode/goproxy/examples/goproxy-transparent

go 1.16

require (
	github.com/gorilla/websocket v1.4.2
	github.com/inconshreveable/go-vhost v0.0.0-20160627193104-06d84117953b
	github.com/mixcode/goproxy v0.0.0-20181111060418-2ce16c963a8a
	github.com/mixcode/goproxy/ext v0.0.0-20210427112856-bd191b4558d9
)

replace github.com/mixcode/goproxy => ../
