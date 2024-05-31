module benchmark

go 1.21.0

replace github.com/urpc/uio => ../

replace github.com/urpc/uio/examples => ../examples

require (
	github.com/lesismal/nbio v1.5.8
	github.com/leslie-fei/ghttp v0.0.0-20240531031732-51a3b64afa0b
	github.com/panjf2000/gnet/v2 v2.5.1
	github.com/urpc/uio v0.0.0-00010101000000-000000000000
	github.com/urpc/uio/examples v0.0.0-00010101000000-000000000000
)

require (
	github.com/antlabs/httparser v0.0.11-0.20231103140926-8998612c074d // indirect
	github.com/lesismal/llib v1.1.13 // indirect
	github.com/libp2p/go-reuseport v0.4.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	go.uber.org/atomic v1.7.0 // indirect
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.21.0 // indirect
	golang.org/x/crypto v0.23.0 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sync v0.7.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.15.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)
