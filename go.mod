module github.com/cr1cr1/o365bridge

go 1.27

require (
	github.com/alecthomas/kong v1.16.1
	github.com/cr1cr1/o365bridge/jmap v0.0.0
	github.com/go-rod/rod v0.116.2
	github.com/joho/godotenv v1.5.1
	github.com/rs/zerolog v1.35.1
)

require (
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ysmood/fetchup v0.2.3 // indirect
	github.com/ysmood/goob v0.4.0 // indirect
	github.com/ysmood/got v0.40.0 // indirect
	github.com/ysmood/gson v0.7.3 // indirect
	github.com/ysmood/leakless v0.9.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/cr1cr1/o365bridge/jmap => ./jmap
