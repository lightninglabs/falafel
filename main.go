package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"text/template"

	"strings"

	"github.com/golang/protobuf/protoc-gen-go/generator"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
	"github.com/grpc-ecosystem/grpc-gateway/codegenerator"
	"github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway/descriptor"
)

const toolName = "falafel"
const version = "0.9.1"

var versionString = fmt.Sprintf("%s %s", toolName, version)

func main() {
	maybeVersion := ""
	if len(os.Args) > 1 {
		maybeVersion = os.Args[1]
	}
	if maybeVersion == "-v" || maybeVersion == "--version" {
		fmt.Println(version)
		return
	}

	// Read the plugin output from protoc.
	req, err := codegenerator.ParseRequest(os.Stdin)
	if err != nil {
		log.Fatal(err)
	}

	// Load the parsed request into a descriptor registry.
	reg := descriptor.NewRegistry()
	if err := reg.Load(req); err != nil {
		log.Fatal(err)
	}

	// Parse the parameters handed to the plugin.
	parameter := req.GetParameter()
	param := split(parameter, ",")

	// Extract the RPC call godoc from the proto.
	godoc := make(map[string]string)
	for _, f := range req.GetProtoFile() {
		fd := &generator.FileDescriptor{
			FileDescriptorProto: f,
		}
		for _, loc := range fd.GetSourceCodeInfo().GetLocation() {
			if loc.LeadingComments == nil {
				continue
			}
			c := *loc.LeadingComments

			// Find the first newline. The actual comment will
			// start following this.
			i := 0
			for j := range c {
				if c[j] == '\n' {
					i = j
					break
				}
			}
			c = c[i+1:]

			// Find the first space. The method's name will
			// be all characters up to that space.
			i = 0
			for j := range c {
				if c[j] == ' ' {
					i = j
					break
				}
			}
			method := c[:i]

			// Insert comment // instead of every newline.
			c = strings.Replace(c, "\n", "\n// ", -1)

			// Remove trailing spaces from comments.
			c = strings.Replace(c, " \n//", "\n//", -1)

			// Add a leading comment // and remove the traling
			// one.
			if len(c) < 4 {
				continue
			}
			c = "// " + c[:len(c)-4]

			godoc[method] = c
		}
	}

	// Go through the requested proto files to generate, and inspect the
	// services they define. We either generate for mobile or for JS.
	if param["js_stubs"] == "1" {
		genJSStubs(param, req, reg)
	} else {
		genMobileStubs(param, godoc, req, reg)
	}

	// Finally, with the service definitions successfully created, create
	// the in memory grpc definitions if requested.
	if param["mem_rpc"] == "1" {
		genMemRPC(param)
	}
}

func genMobileStubs(param, godoc map[string]string,
	req *plugin.CodeGeneratorRequest, reg *descriptor.Registry) {

	// We need package_name and target_package in order to continue.
	pkg := param["package_name"]
	if pkg == "" {
		log.Fatal("package name not set")
	}

	// Further split the listener params by service name. They come in the
	// following format:
	// listeners=[service1=lis1 service2=lis2]
	lis := param["listeners"]
	listeners := split(lis, " ")

	// For services where the listener is not specified, we can use a
	// default listener if provided.
	defaultLis := param["defaultlistener"]

	targetPkg := param["target_package"]
	if targetPkg == "" {
		log.Fatal("target package not set")
	}

	targetName := ""
	if i := strings.LastIndex(targetPkg, "/"); i > 0 {
		targetName = targetPkg[i+1:]
	}

	buildTags := param["build_tags"]

	apiPrefix := false
	if param["api_prefix"] == "1" {
		apiPrefix = true
	}

	for _, filename := range req.FileToGenerate {
		target, err := reg.LookupFile(filename)
		if err != nil {
			log.Fatal(err)
		}

		// For each service, we'll create a file with the generated api.
		for _, s := range target.Services {
			name := s.GetName()
			n := strings.ToLower(name)

			listener := listeners[n]
			if listener == "" {
				if defaultLis == "" {
					log.Fatal(fmt.Sprintf("no listener set for "+
						"service %s", n))
				}
				listener = defaultLis
			}

			f, err := os.Create("./" + n + "_api_generated.go")
			if err != nil {
				log.Fatal(err)
			}

			wr := bufio.NewWriter(f)

			// Create the file header.
			params := headerParams{
				ToolName:  versionString,
				FileName:  filename,
				Package:   pkg,
				TargetPkg: targetPkg,
				BuildTags: buildTags,
			}
			if err := headerTemplate.Execute(wr, params); err != nil {
				log.Fatal(err)
			}

			// Create service specific methods.
			p := serviceParams{
				ServiceName: name,
				TargetName:  targetName,
				Listener:    listener,
			}
			if err := serviceTemplate.Execute(wr, p); err != nil {
				log.Fatal(err)
			}

			// Go through each method defined by the service, and
			// call the appropriate template, depending on the RPC
			// type.
			for _, m := range s.Methods {
				name := m.GetName()
				p := rpcParams{
					ServiceName: s.GetName(),
					MethodName:  name,
					// Type names are returned with an
					// initial dot, e.g.
					// .lnrpc.GetInfoRequest, we just strip
					// that dot away.
					RequestType: m.GetInputType()[1:],
					Comment:     godoc[name],
				}
				if apiPrefix {
					p.ApiPrefix = p.ServiceName
				}

				clientStream := false
				serverStream := false
				if m.ClientStreaming != nil {
					clientStream = *m.ClientStreaming
				}

				if m.ServerStreaming != nil {
					serverStream = *m.ServerStreaming
				}

				switch {
				case !clientStream && !serverStream:
					if err := syncTemplate.Execute(wr, p); err != nil {
						log.Fatal(err)
					}

				case !clientStream && serverStream:
					if err := readStreamTemplate.Execute(wr, p); err != nil {
						log.Fatal(err)
					}

				case clientStream && serverStream:
					if err := biStreamTemplate.Execute(wr, p); err != nil {
						log.Fatal(err)
					}

				default:
					log.Fatal("unexpected method type")
				}
			}

			if err := wr.Flush(); err != nil {
				log.Fatal(err)
			}
			if err := f.Close(); err != nil {
				log.Fatal(err)
			}
		}
	}
}

func genJSStubs(param map[string]string, req *plugin.CodeGeneratorRequest,
	reg *descriptor.Registry) {

	// We need package_name and target_package in order to continue.
	pkg := param["package_name"]
	if pkg == "" {
		log.Fatal("package name not set")
	}

	buildTag := param["build_tags"]
	manualImport := param["manual_import"]

	for _, filename := range req.FileToGenerate {
		target, err := reg.LookupFile(filename)
		if err != nil {
			log.Fatal(err)
		}

		// For each service, we'll create a file with the generated api.
		for _, s := range target.Services {
			name := s.GetName()
			n := strings.ToLower(name)

			f, err := os.Create("./" + n + ".pb.json.go")
			if err != nil {
				log.Fatal(err)
			}

			wr := bufio.NewWriter(f)

			// Create the file header.
			params := jsHeaderParams{
				ToolName:     versionString,
				FileName:     filename,
				ServiceName:  name,
				Package:      pkg,
				ManualImport: manualImport,
				BuildTag:     buildTag,
			}

			// Go through each method defined by the service, and
			// call the appropriate template, depending on the RPC
			// type.
			for _, m := range s.Methods {
				name := m.GetName()

				// Type names are returned with an initial dot,
				// e.g. .lnrpc.GetInfoRequest, we just strip
				// that dot away.
				inputType := m.GetInputType()[1:]
				if strings.Contains(inputType, pkg) {
					inputType = strings.ReplaceAll(
						inputType, pkg+".", "",
					)
				}

				p := jsRpcParams{
					MethodName:  name,
					ServiceName: s.GetName(),
					RequestType: inputType,
				}

				clientStream := false
				if m.ClientStreaming != nil {
					clientStream = *m.ClientStreaming
				}

				if m.ServerStreaming != nil {
					p.ResponseStreaming = *m.ServerStreaming
				}

				if clientStream {
					continue
				}

				params.Methods = append(params.Methods, p)
			}

			if err := jsTemplate.Execute(wr, params); err != nil {
				log.Fatal(err)
			}

			if err := wr.Flush(); err != nil {
				log.Fatal(err)
			}
			if err := f.Close(); err != nil {
				log.Fatal(err)
			}
		}
	}
}

func genMemRPC(param map[string]string) {
	// We need package_name and target_package in order to continue.
	pkg := param["package_name"]
	if pkg == "" {
		log.Fatal("package name not set")
	}

	// Further split the listener params by service name. They come in the
	// following format:
	// listeners=[service1=lis1 service2=lis2]
	lis := param["listeners"]
	listeners := split(lis, " ")

	var (
		usedListeners []string
		added         = make(map[string]struct{})
	)
	for _, listener := range listeners {
		// Skip listeners already added to the slice, to avoid
		// the definitions being created multiple times.
		if _, ok := added[listener]; ok {
			continue
		}

		usedListeners = append(usedListeners, listener)
		added[listener] = struct{}{}
	}

	f, err := os.Create("./memrpc_generated.go")
	if err != nil {
		log.Fatal(err)
	}

	wr := bufio.NewWriter(f)
	p := memRpcParams{
		ToolName: versionString,
		Package:  pkg,
	}
	if err := memRpcTemplate.Execute(wr, p); err != nil {
		log.Fatal(err)
	}
	if err := wr.Flush(); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	lisf, err := os.Create("./listeners_generated.go")
	if err != nil {
		log.Fatal(err)
	}

	liswr := bufio.NewWriter(lisf)
	lisp := listenersParams{
		ToolName:  versionString,
		Package:   pkg,
		Listeners: usedListeners,
	}
	err = listenersTemplate.Execute(liswr, lisp)
	if err != nil {
		log.Fatal(err)
	}
	if err := liswr.Flush(); err != nil {
		log.Fatal(err)
	}
	if err := lisf.Close(); err != nil {
		log.Fatal(err)
	}
}

func split(parameter string, c string) map[string]string {
	param := make(map[string]string)
	if parameter == "" {
		return param
	}

	for _, p := range strings.Split(parameter, c) {
		if i := strings.Index(p, "="); i < 0 {
			param[p] = ""
		} else {
			param[p[0:i]] = p[i+1:]
		}
	}
	return param
}

var funcMap = template.FuncMap{
	"LowerCase": lowerCase,
	"UpperCase": upperCase,
}

func lowerCase(s string) string {
	if s == "" {
		return ""
	}

	return strings.ToLower(s[:1]) + s[1:]
}

func upperCase(s string) string {
	if s == "" {
		return ""
	}

	return strings.ToUpper(s[:1]) + s[1:]
}
