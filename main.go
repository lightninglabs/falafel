package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"text/template"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
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

	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		// Set support for optional fields in proto3
		gen.SupportedFeatures = uint64(
			pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL,
		)

		// Parse the parameters handed to the plugin.
		param := parseParams(gen.Request.GetParameter())

		// Iterate over each file passed to the plugin.
		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}

			// Extract the RPC call godoc from the proto file.
			godoc := extractComments(f)

			// Generate stubs either for mobile or for JS.
			if param["js_stubs"] == "1" {
				genJSStubs(gen, f, param)
			} else {
				genMobileStubs(gen, f, param, godoc)
			}

			// Finally, with the service definitions successfully
			// created, create the in-memory grpc definitions if
			// requested.
			if param["mem_rpc"] == "1" {
				genMemRPC(gen, f, param)
			}
		}

		return nil
	})
}

// parseParams parses any parameters handed to the plugin.
func parseParams(parameter string) map[string]string {
	param := make(map[string]string)
	if parameter == "" {
		return param
	}

	for _, p := range strings.Split(parameter, ",") {
		if i := strings.Index(p, "="); i < 0 {
			param[p] = ""
		} else {
			param[p[0:i]] = p[i+1:]
		}
	}

	return param
}

// extractComments extracts the RPC call godoc from the proto file.
func extractComments(file *protogen.File) map[string]string {
	locations := file.Desc.SourceLocations()

	godoc := make(map[string]string)
	for i := 0; i < locations.Len(); i++ {
		loc := locations.Get(i)

		if loc.LeadingComments == "" {
			continue
		}
		c := loc.LeadingComments

		// Find the first newline. The actual comment will start
		// following this.
		i := 0
		for j := range c {
			if c[j] == '\n' {
				i = j
				break
			}
		}
		c = c[i+1:]

		// Find the first space. The method's name will be all
		// characters up to that space.
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

		// Add a leading comment // and remove the trailing one.
		if len(c) < 4 {
			continue
		}
		c = "// " + c[:len(c)-4]

		godoc[method] = c
	}

	return godoc
}

func genMobileStubs(gen *protogen.Plugin, file *protogen.File,
	param map[string]string, godoc map[string]string) {

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

	// For each service, we'll create a file with the generated API.
	for _, service := range file.Services {
		name := service.GoName
		n := strings.ToLower(name)

		listener := listeners[n]
		if listener == "" {
			if defaultLis == "" {
				log.Fatal(fmt.Sprintf("no listener set for "+
					"service %s", n))
			}
			listener = defaultLis
		}

		filename := "./" + n + "_api_generated.go"
		g := gen.NewGeneratedFile(filename, file.GoImportPath)

		// Create the file header.
		params := headerParams{
			ToolName:  versionString,
			FileName:  filename,
			Package:   pkg,
			TargetPkg: targetPkg,
			BuildTags: buildTags,
		}
		if err := headerTemplate.Execute(g, params); err != nil {
			log.Fatal(err)
		}

		// Create service specific methods.
		serviceParams := serviceParams{
			ServiceName: name,
			TargetName:  targetName,
			Listener:    listener,
		}
		err := serviceTemplate.Execute(g, serviceParams)
		if err != nil {
			log.Fatal(err)
		}

		// Go through each method defined by the service and call the
		// appropriate template depending on the RPC type.
		for _, method := range service.Methods {
			methodName := method.GoName

			// Get the input type's package.
			typeImportPath := string(
				method.Input.GoIdent.GoImportPath,
			)
			path := strings.Split(typeImportPath, "/")
			if len(path) == 0 {
				log.Fatal("expected an import path for the " +
					"input type but got none")
				return
			}

			// Get the package name of the input type.
			inputPkg := path[len(path)-1]

			inputType := method.Input.GoIdent.GoName

			// If the input comes from an outside package, we need
			// to prepend the outside package's name to the type.
			if inputPkg != pkg {
				inputType = fmt.Sprintf(
					"%s.%s", inputPkg, inputType,
				)
			}

			rpcParams := rpcParams{
				ServiceName: service.GoName,
				MethodName:  methodName,
				RequestType: inputType,
				Comment:     godoc[methodName],
			}
			if apiPrefix {
				rpcParams.ApiPrefix = service.GoName
			}

			clientStream := method.Desc.IsStreamingClient()
			serverStream := method.Desc.IsStreamingServer()

			switch {
			case !clientStream && !serverStream:
				err := syncTemplate.Execute(g, rpcParams)
				if err != nil {
					log.Fatal(err)
				}

			case !clientStream && serverStream:
				err := readStreamTemplate.Execute(g, rpcParams)
				if err != nil {
					log.Fatal(err)
				}

			case clientStream && serverStream:
				err := biStreamTemplate.Execute(g, rpcParams)
				if err != nil {
					log.Fatal(err)
				}

			default:
				log.Fatal("unexpected method type")
			}
		}
	}
}

func genJSStubs(gen *protogen.Plugin, file *protogen.File,
	param map[string]string) {

	// We need package_name and target_package in order to continue.
	pkg := param["package_name"]
	if pkg == "" {
		log.Fatal("package name not set")
	}

	buildTag := param["build_tags"]
	manualImport := param["manual_import"]

	// For each service, we'll create a file with the generated API.
	for _, service := range file.Services {
		name := service.GoName
		n := strings.ToLower(name)

		filename := "./" + n + ".pb.json.go"
		g := gen.NewGeneratedFile(filename, file.GoImportPath)

		// Create the file header.
		params := jsHeaderParams{
			ToolName:     versionString,
			FileName:     file.Proto.GetName(),
			ServiceName:  name,
			Package:      pkg,
			ManualImport: manualImport,
			BuildTag:     buildTag,
		}

		// Go through each method defined by the service and call the
		// appropriate template.
		for _, method := range service.Methods {
			methodName := method.GoName

			// Get the input type's package.
			path := strings.Split(
				string(method.Input.GoIdent.GoImportPath), "/",
			)
			if len(path) == 0 {
				log.Fatal("expected an import path for the " +
					"input type but got none")
				return
			}

			// Get the package name of the input type.
			inputPkg := path[len(path)-1]

			inputType := method.Input.GoIdent.GoName

			// If the input comes from an outside package, we need
			// to prepend the outside package's name to the type.
			// This will likely be a "manual_import" that the user
			// has specified.
			// TODO: remove the need for manual import?
			if inputPkg != pkg {
				inputType = fmt.Sprintf(
					"%s.%s", inputPkg, inputType,
				)
			}

			p := jsRpcParams{
				MethodName:  methodName,
				ServiceName: service.GoName,
				RequestType: inputType,
			}

			clientStream := method.Desc.IsStreamingClient()
			serverStream := method.Desc.IsStreamingServer()

			if serverStream {
				p.ResponseStreaming = true
			}

			if clientStream {
				continue
			}

			params.Methods = append(params.Methods, p)
		}

		if err := jsTemplate.Execute(g, params); err != nil {
			log.Fatal(err)
		}
	}
}

func genMemRPC(gen *protogen.Plugin, file *protogen.File,
	param map[string]string) {

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

	// Create memrpc_generated.go file
	filename := "./memrpc_generated.go"
	g := gen.NewGeneratedFile(filename, file.GoImportPath)
	p := memRpcParams{
		ToolName: versionString,
		Package:  pkg,
	}
	if err := memRpcTemplate.Execute(g, p); err != nil {
		log.Fatal(err)
	}

	// Create listeners_generated.go file
	lisFilename := "./listeners_generated.go"
	lisG := gen.NewGeneratedFile(lisFilename, file.GoImportPath)
	lisp := listenersParams{
		ToolName:  versionString,
		Package:   pkg,
		Listeners: usedListeners,
	}
	if err := listenersTemplate.Execute(lisG, lisp); err != nil {
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
