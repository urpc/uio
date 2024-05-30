module github.com/urpc/uio/examples

go 1.21.0

replace github.com/urpc/uio => ../

require (
	github.com/antlabs/httparser v0.0.11-0.20231103140926-8998612c074d
	github.com/urpc/uio v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.25.0
)

require (
	github.com/libp2p/go-reuseport v0.4.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.15.0 // indirect
)
