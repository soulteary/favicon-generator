module github.com/soulteary/favicon-generator

go 1.21.2

replace github.com/willnorris/gifresize => ./pkg/willnorris/gifresize

replace github.com/willnorris/imageproxy => ./pkg/willnorris/imageproxy

replace github.com/nfnt/resize => ./pkg/nfnt/resize

replace github.com/rwcarlsen/goexif => ./pkg/rwcarlsen/goexif

require (
	github.com/die-net/lrucache v0.0.0-20220628165024-20a71bc65bf1
	github.com/gregjones/httpcache v0.0.0-20190611155906-901d90724c79
	github.com/peterbourgon/diskv v2.0.1+incompatible
	github.com/willnorris/imageproxy v0.0.0-00010101000000-000000000000
)

require (
	github.com/disintegration/imaging v1.6.2 // indirect
	github.com/fcjr/aia-transport-go v1.2.2 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/muesli/smartcrop v0.3.0 // indirect
	github.com/nfnt/resize v0.0.0-20180221191011-83c6a9932646 // indirect
	github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd // indirect
	github.com/willnorris/gifresize v1.0.0 // indirect
	golang.org/x/image v0.14.0 // indirect
)
