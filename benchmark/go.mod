module benchmark

go 1.21.0

replace github.com/urpc/uio => ../

replace github.com/urpc/uio/examples => ../examples

require (
	github.com/lesismal/nbio v1.5.8
	github.com/urpc/uio v0.0.0-00010101000000-000000000000
	github.com/urpc/uio/examples v0.0.0-00010101000000-000000000000
)

require (
	github.com/antlabs/httparser v0.0.11-0.20231103140926-8998612c074d // indirect
	github.com/lesismal/llib v1.1.13 // indirect
	github.com/libp2p/go-reuseport v0.4.0 // indirect
	github.com/stretchr/testify v1.8.4 // indirect
	golang.org/x/crypto v0.23.0 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.15.0 // indirect
)
