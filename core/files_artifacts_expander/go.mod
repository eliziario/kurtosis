module github.com/kurtosis-tech/kurtosis/core/files_artifacts_expander

go 1.17

replace github.com/kurtosis-tech/kurtosis/api/golang => ../../api/golang

require (
	github.com/gammazero/workerpool v1.1.2
	github.com/kurtosis-tech/kurtosis/api/golang v0.0.0
	github.com/kurtosis-tech/stacktrace v0.0.0-20211028211901-1c67a77b5409
	github.com/sirupsen/logrus v1.8.1
	google.golang.org/grpc v1.38.0
)

require (
	github.com/gammazero/deque v0.1.0 // indirect
	github.com/golang/protobuf v1.5.2 // indirect
	golang.org/x/net v0.0.0-20190311183353-d8887717615a // indirect
	golang.org/x/sys v0.0.0-20220722155257-8c9f86f7a55f // indirect
	golang.org/x/text v0.3.8 // indirect
	google.golang.org/genproto v0.0.0-20200526211855-cb27e3aa2013 // indirect
	google.golang.org/protobuf v1.27.1 // indirect
)
