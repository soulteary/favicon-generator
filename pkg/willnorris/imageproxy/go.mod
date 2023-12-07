module github.com/willnorris/imageproxy

go 1.21.2

require (
	github.com/disintegration/imaging v1.6.2
	github.com/fcjr/aia-transport-go v1.2.2
	github.com/gregjones/httpcache v0.0.0-20190611155906-901d90724c79
	github.com/muesli/smartcrop v0.3.0
	github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd
	github.com/willnorris/gifresize v1.0.0
	golang.org/x/image v0.14.0
)

require github.com/nfnt/resize v0.0.0-20180221191011-83c6a9932646 // indirect

replace github.com/gregjones/httpcache => ../../gregjones/httpcache

replace github.com/fcjr/aia-transport-go => ../../fcjr/aia-transport-go

replace github.com/disintegration/imaging => ../../disintegration/imaging

replace github.com/nfnt/resize => ../../nfnt/resize

replace github.com/rwcarlsen/goexif => ../../rwcarlsen/goexif

replace github.com/muesli/smartcrop => ../../muesli/smartcrop

replace github.com/willnorris/gifresize => ../gifresize
