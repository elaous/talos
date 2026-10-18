module github.com/elaous/talos

go 1.26.0

require (
	github.com/elaous/gojiberry v0.0.0
	golang.org/x/net v0.61.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/dslipak/pdf v0.0.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/phpdave11/gofpdi v1.0.14-0.20211212211723-1f10f9844311 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/signintech/gopdf v0.37.0 // indirect
	golang.org/x/image v0.44.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

// gojiberry is private; consume it from a sibling checkout (see its README, "Getting Started").
replace github.com/elaous/gojiberry => ../gojiberry
