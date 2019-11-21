# falafel
falafel is a `protoc` plugin written in go that is used to generate
[`gomobile`](https://godoc.org/golang.org/x/mobile/cmd/gomobile) compatible
APIs for gRPC services for use on mobile platforms.

Currently being used with
[lnd](https://github.com/lightningnetwork/lnd/tree/master/mobile).

### Description
falafel translates protobuf definitions to `gomobile` compatible APIs. Behind
this API we directly talk to the gRPC server using an in-memory gRPC client,
ensuring all communication happens in-process using serialized protocol
buffers, without needing to expose the gRPC server on an open port. To support
streaming RPCs, like subscribing to real-time updates, callbacks are provided
for all APIs.

The gRPC server must support using custom listeners.

### Getting started Pass the falafel plugin to `protoc` with custom options.
Here is an example how `falafel` is used with `lnd`:

```bash
falafel=$(which falafel)

# Name of the package for the generated APIs.
pkg="lndmobile"

# The package where the protobuf definitions originally are found.
target_pkg="github.com/lightningnetwork/lnd/lnrpc"

# A mapping from grpc service to name of the custom listeners. The grpc server
# must be configured to listen on these.
listeners="lightning=lightningLis walletunlocker=walletUnlockerLis"

# Set to 1 to create boiler plate grpc client code and listeners. If more than
# one proto file is being parsed, it should only be done once.
mem_rpc=1

opts="package_name=$pkg,target_package=$target_pkg,listeners=$listeners,mem_rpc=$mem_rpc"
protoc -I/usr/local/include -I. \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --plugin=protoc-gen-custom=$falafel\
       --custom_out=./build \
       --custom_opt="$opts" \
       --proto_path=../lnrpc \
       rpc.proto
```

With the go bindings generated, define an entry point for the application to
start the gRPC service:

```go
func Start() {
	// We call the main method with the custom in-memory listeners called
	// by the mobile APIs, such that the grpc server will use these.
	cfg := lnd.ListenerCfg{
		WalletUnlocker: walletUnlockerLis,
		RPCListener:    lightningLis,
	}

	go func() {
		if err := lnd.Main(cfg); err != nil {
			if e, ok := err.(*flags.Error); ok &&
				e.Type == flags.ErrHelp {
			} else {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(1)
		}
	}()
}
```

The gRPC server should be started by listening on the passed listeners.

### Compiling with gomobile
Package `lndmobile` is now ready to be cross-compiled using `gomobile`:
```bash
gomobile bind -target=ios github.com/lightningnetwork/lnd/mobile
```
