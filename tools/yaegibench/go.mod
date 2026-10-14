module github.com/gabay/maxmind-geoblock/tools/yaegibench

go 1.22

require (
	github.com/gabay/maxmind-geoblock v0.0.0
	github.com/mitchellh/mapstructure v1.5.0
	github.com/traefik/yaegi v0.16.1
)

replace github.com/gabay/maxmind-geoblock => ../..
