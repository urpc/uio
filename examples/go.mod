module github.com/urpc/uio/examples

go 1.21.0

replace github.com/urpc/uio => ../

require (
	github.com/antlabs/httparser v0.0.11
	github.com/urpc/uio v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.30.0
)

require (
	github.com/libp2p/go-reuseport v0.4.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/text v0.19.0 // indirect
)
