module github.com/consensys/gnark

go 1.17

require (
	github.com/consensys/bavard v0.1.13
	github.com/consensys/gnark-crypto v0.7.0
	github.com/fxamacker/cbor/v2 v2.2.0
	github.com/leanovate/gopter v0.2.9
	github.com/rs/zerolog v1.26.1
	github.com/stretchr/testify v1.8.2
	github.com/sunblaze-ucb/simpleMPI v0.0.0-20221128060547-885514bf9648
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mmcloughlin/addchain v0.4.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/crypto v0.17.0 // indirect
	golang.org/x/sys v0.15.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	rsc.io/tmplfunc v0.0.3 // indirect
)

replace github.com/consensys/gnark-crypto => ../pianist-gnark-crypto
