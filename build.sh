#!/bin/sh

generate() {
	local os="$1"
	local arch="$2"
	local dir="rttys-$os-$arch"
	local bin="rttys"

	mkdir output/$dir
	cp output/rttys.crt output/rttys.key output/$dir

	[ "$os" = "windows" ] && {
		bin="rttys.exe"
	}

	GOOS=$os GOARCH=$arch go build -ldflags='-s -w' -o output/$dir/$bin

	cd output

	if [ "$os" = "windows" ];
	then
		zip -r $dir.zip $dir
	else
		tar zcvf $dir.tar.gz $dir
	fi

	cd ..
}

rm -rf output
mkdir output

TARGET=output ./generate-CA.sh rttys

generate linux amd64
generate linux 386

generate windows amd64
generate windows 386
