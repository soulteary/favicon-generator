module github.com/soulteary/favicon-generator

go 1.27.3

replace github.com/willnorris/imageproxy => ./pkg/willnorris/imageproxy

replace github.com/willnorris/gifresize => ./pkg/willnorris/gifresize

require (
	github.com/die-net/lrucache v0.0.0-20240714232319-26322ba4bc23
	github.com/willnorris/imageproxy v0.0.0
)

require (
	github.com/disintegration/imaging v1.6.3-0.20201218193011-d40f48ce0f09 // indirect
	github.com/gregjones/httpcache v0.0.0-20190611155906-901d90724c79 // indirect
	github.com/muesli/smartcrop v0.3.1-0.20230316170055-f1935b108c21 // indirect
	github.com/nfnt/resize v0.0.0-20180221191011-83c6a9932646 // indirect
	github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd // indirect
	github.com/willnorris/gifresize v1.0.0 // indirect
	golang.org/x/image v0.47.0 // indirect
)
