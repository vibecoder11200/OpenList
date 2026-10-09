![Logo](/GoFakeS3.png)

[![Build Status](https://github.com/OpenListTeam/gofakes3/workflows/build/badge.svg)](https://github.com/OpenListTeam/gofakes3/actions?query=workflow%3Abuild)
[![Go Report Card](https://goreportcard.com/badge/github.com/OpenListTeam/gofakes3)](https://goreportcard.com/report/github.com/OpenListTeam/gofakes3)
[![GoDoc](https://pkg.go.dev/badge/github.com/OpenListTeam/gofakes3.svg)](https://pkg.go.dev/github.com/OpenListTeam/gofakes3)

This is a fork of [johannesboyne/gofakes3](https://github.com/johannesboyne/gofakes3)
mainly for use implementing the [OpenList serves3](https://doc.oplist.org/guide/advanced/s3) in
[OpenListTeam/OpenList](https://github.com/OpenListTeam/OpenList).

Notable differences:

* Use modified xml library to handle more control chars
* Func `getVersioningConfiguration` will return empty when unversioned
* New func in `backend` interface: `CopyObject`
* Support authentication with AWS Signature V4 
* Interfaces changed to take context
