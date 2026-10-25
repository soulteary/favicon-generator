module github.com/soulteary/favicon-generator

go 1.27.3

replace github.com/willnorris/imageproxy => ./pkg/willnorris/imageproxy

replace github.com/willnorris/gifresize => ./pkg/willnorris/gifresize

require (
	github.com/die-net/lrucache v0.0.0-20240714232319-26322ba4bc23
	github.com/prometheus/client_golang v1.24.1
	github.com/soulteary/cli-kit v1.9.2
	github.com/soulteary/health-kit/v4 v4.0.3
	github.com/soulteary/logger-kit/v3 v3.0.3
	github.com/soulteary/metrics-kit/v3 v3.0.3
	github.com/soulteary/version-kit/v4 v4.0.2
	github.com/soulteary/vfs-kit v1.4.6
	github.com/willnorris/imageproxy v0.0.0
	golang.org/x/image v0.47.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/disintegration/imaging v1.6.3-0.20201218193011-d40f48ce0f09 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gregjones/httpcache v0.0.0-20190611155906-901d90724c79 // indirect
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/muesli/smartcrop v0.3.1-0.20230316170055-f1935b108c21 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nfnt/resize v0.0.0-20180221191011-83c6a9932646 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.1 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd // indirect
	github.com/willnorris/gifresize v1.0.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
