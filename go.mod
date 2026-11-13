module virticle

go 1.26.0

require (
	filippo.io/age v1.3.2
	github.com/coder/websocket v1.8.15
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/digitalocean/go-libvirt v0.0.0-20261013170128-efbbdf578ac8
	github.com/go-asn1-ber/asn1-ber v1.5.8
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/go-ldap/ldap/v3 v3.4.15
	github.com/makiuchi-d/gozxing v0.1.1
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	golang.org/x/crypto v0.58.0
	golang.org/x/oauth2 v0.38.0
	golang.org/x/term v0.48.0
	modernc.org/sqlite v1.62.0
)

require filippo.io/hpke v0.4.0 // indirect

require (
	github.com/Azure/go-ntlmssp v0.1.1 // indirect
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.50.0
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.80.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace github.com/digitalocean/go-libvirt => ./third_party/go-libvirt
