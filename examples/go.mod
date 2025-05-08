module github.com/urpc/uio/examples

go 1.23.0

replace github.com/urpc/uio => ../

require (
	github.com/antlabs/httparser v0.0.11
	github.com/urpc/uio v0.0.0-20250506164505-ab569f41e6a0
	golang.org/x/net v0.40.0
)

require (
	github.com/libp2p/go-reuseport v0.4.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.25.0 // indirect
)
