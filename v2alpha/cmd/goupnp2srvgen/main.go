package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"
	"github.com/huin/goupnp/v2alpha/cmd/goupnp2srvgen/tmplfuncs"
	"github.com/huin/goupnp/v2alpha/cmd/goupnp2srvgen/zipread"
	"github.com/huin/goupnp/v2alpha/description/srvdesc"
	"github.com/huin/goupnp/v2alpha/description/typedesc"
	"github.com/huin/goupnp/v2alpha/description/xmlsrvdesc"
	"github.com/huin/goupnp/v2alpha/soap"
	"github.com/huin/goupnp/v2alpha/soap/types"
)

var (
	formatOutput     = flag.Bool("format_output", true, "If true, format the output source code.")
	outputDir        = flag.String("output_dir", "", "Path to directory to write output in.")
	srvManifests     = flag.String("srv_manifests", "", "Path to srvmanifests.toml")
	srvTemplate      = flag.String("srv_template", "", "Path to srv.gotemplate.")
	upnpresourcesZip = flag.String("upnpresources_zip", "", "Path to upnpresources.zip.")
)

const soapActionInterface = "SOAPActionInterface"

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(flag.Args()) > 0 {
		return fmt.Errorf("unused arguments: %s", strings.Join(flag.Args(), " "))
	}

	if *outputDir == "" {
		return errors.New("-output_dir is a required flag.")
	}
	if err := os.MkdirAll(*outputDir, 0); err != nil {
		return fmt.Errorf("creating output_dir %q: %w", *outputDir, err)
	}

	if *srvManifests == "" {
		return errors.New("-srv_manifests is a required flag.")
	}
	var manifests DCPSpecManifests
	_, err := toml.DecodeFile(*srvManifests, &manifests)
	if err != nil {
		return fmt.Errorf("loading srv_manifests %q: %w", *srvManifests, err)
	}

	if *srvTemplate == "" {
		return errors.New("-srv_template is a required flag.")
	}
	tmpl, err := template.New(filepath.Base(*srvTemplate)).Funcs(template.FuncMap{
		"args":  tmplfuncs.Args,
		"quote": strconv.Quote,
	}).ParseFiles(*srvTemplate)
	if err != nil {
		return fmt.Errorf("loading srv_template %q: %w", *srvTemplate, err)
	}

	if *upnpresourcesZip == "" {
		return errors.New("-upnpresources_zip is a required flag.")
	}
	f, err := os.Open(*upnpresourcesZip)
	if err != nil {
		return err
	}
	defer f.Close()
	upnpresources, err := zipread.FromOsFile(f)
	if err != nil {
		return err
	}

	// Use default type map for now. Addtional types could be use instead or
	// as well as necessary for extended types.
	typeMap := types.TypeMap().Clone()
	typeMap[soapActionInterface] = typedesc.TypeDesc{
		GoType: reflect.TypeOf((*soap.Action)(nil)).Elem(),
	}

	for _, m := range manifests.DCPS {
		if err := processDCP(upnpresources, m, typeMap, tmpl, *outputDir); err != nil {
			return fmt.Errorf("processing DCP %s: %w", m.SpecZipPath, err)
		}
	}
	return nil
}

func processDCP(
	upnpresources *zipread.ZipRead,
	manifest *DCPSpecManifest,
	typeMap typedesc.TypeMap,
	tmpl *template.Template,
	parentOutputDir string,
) error {
	outputDir := filepath.Join(parentOutputDir, manifest.OutputDir)
	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		return fmt.Errorf("creating output directory %q for DCP: %w", outputDir, err)
	}
	dcpSpecData, err := upnpresources.OpenZip(manifest.SpecZipPath)
	if err != nil {
		return err
	}
	for _, srvManifest := range manifest.Services {
		if err := processService(dcpSpecData, srvManifest, typeMap, tmpl, outputDir); err != nil {
			return fmt.Errorf("processing service %s: %w", srvManifest.ServiceType, err)
		}
	}
	return nil
}

func processService(
	dcpSpecData *zipread.ZipRead,
	srvManifest *ServiceManifest,
	typeMap typedesc.TypeMap,
	tmpl *template.Template,
	parentOutputDir string,
) error {
	outputDir := filepath.Join(parentOutputDir, srvManifest.Package)
	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		return fmt.Errorf("creating output directory %q for service: %w", outputDir, err)
	}

	f, err := dcpSpecData.Open(srvManifest.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	d := xml.NewDecoder(f)

	xmlSCPD := &xmlsrvdesc.SCPD{}
	if err := d.Decode(xmlSCPD); err != nil {
		return err
	}
	xmlSCPD.Clean()

	sd, err := srvdesc.FromXML(xmlSCPD)
	if err != nil {
		return fmt.Errorf("transforming service description: %w", err)
	}

	imps, err := accumulateImports(sd, typeMap)
	if err != nil {
		return err
	}

	buf := &bytes.Buffer{}
	err = tmpl.ExecuteTemplate(buf, "service", tmplArgs{
		Manifest: srvManifest,
		Imps:     imps,
		SCPD:     sd,
	})
	if err != nil {
		return fmt.Errorf("executing srv_template: %w", err)
	}
	src := buf.Bytes()
	if *formatOutput {
		var err error
		src, err = format.Source(src)
		if err != nil {
			return fmt.Errorf("formatting output service file: %w", err)
		}
	}

	outputPath := filepath.Join(outputDir, srvManifest.Package+".go")
	if err := ioutil.WriteFile(outputPath, src, os.ModePerm); err != nil {
		return fmt.Errorf("writing output service file %q: %w", outputPath, err)
	}

	return nil
}

type DCPSpecManifests struct {
	DCPS []*DCPSpecManifest `toml:"dcp"`
}

type DCPSpecManifest struct {
	// SpecZipPath is the file path within upnpresources.zip to the DCP spec ZIP file.
	SpecZipPath string `toml:"spec_zip_path"`
	// OutputDir is the path relative to --output_dir which the packages are written in.
	OutputDir string `toml:"output_dir"`
	// Services maps from a service name (e.g. "FooBar:1") to a path within the DCP spec ZIP file
	// (e.g. "xml data files/service/FooBar1.xml").
	Services []*ServiceManifest `toml:"service"`
}

type ServiceManifest struct {
	// Package is the Go package name to generate e.g. "foo1".
	Package string `toml:"package"`
	// ServiceType is the SOAP namespace and service type that identifes the service e.g.
	// "urn:schemas-upnp-org:service:Foo:1"
	ServiceType string `toml:"type"`
	// Path within the DCP spec ZIP file e.g. "xml data files/service/Foo1.xml".
	Path string `toml:"path"`

	// DocumentURL is the URL to the documentation for the service.
	DocumentURL string `toml:"document_url"`
}

type tmplArgs struct {
	Manifest *ServiceManifest
	Imps     *imports
	SCPD     *srvdesc.SCPD
}

type imports struct {
	// Maps from a type name like "ui4" to the `alias.name` for the import.
	TypeByName map[string]typeDesc
	// Each required import line, ordered by path.
	ImportLines []importItem
}

type typeDesc struct {
	// How to refer to the type, e.g. `pkg.Name`.
	Ref string
	// How to refer to the type absolutely (but not valid Go), e.g.
	// `"github.com/foo/bar/pkg".Name`.
	AbsRef string
	// Name of the type without package, e.g. `Name`.
	Name string
}

type importItem struct {
	Alias string
	Path  string
}

func accumulateImports(srvDesc *srvdesc.SCPD, typeMap typedesc.TypeMap) (*imports, error) {
	typeNames := make(map[string]bool)
	typeNames[soapActionInterface] = true

	err := visitTypesSCPD(srvDesc, func(typeName string) {
		typeNames[typeName] = true
	})
	if err != nil {
		return nil, err
	}

	// Have sorted list of import package paths. Partly for aesthetics of generated code, but also
	// to have stable-generated aliases.
	paths := make(map[string]bool)
	for typeName := range typeNames {
		t, ok := typeMap[typeName]
		if !ok {
			return nil, fmt.Errorf("unknown type %q", typeName)
		}
		pkgPath := t.GoType.PkgPath()
		if pkgPath == "" {
			// Builtin type, ignore.
			continue
		}
		paths[pkgPath] = true
	}
	sortedPaths := make([]string, 0, len(paths))
	for path := range paths {
		sortedPaths = append(sortedPaths, path)
	}
	sort.Strings(sortedPaths)

	// Generate import aliases.
	index := 1
	aliasByPath := make(map[string]string, len(paths))
	importLines := make([]importItem, 0, len(paths))
	for _, path := range sortedPaths {
		alias := fmt.Sprintf("pkg%d", index)
		index++
		importLines = append(importLines, importItem{
			Alias: alias,
			Path:  path,
		})
		aliasByPath[path] = alias
	}

	// Populate typeByName.
	typeByName := make(map[string]typeDesc, len(typeNames))
	for typeName := range typeNames {
		goType := typeMap[typeName]
		pkgPath := goType.GoType.PkgPath()
		alias := aliasByPath[pkgPath]
		td := typeDesc{
			Name: goType.GoType.Name(),
		}
		if alias == "" {
			// Builtin type.
			td.AbsRef = td.Name
			td.Ref = td.Name
		} else {
			td.AbsRef = strconv.Quote(pkgPath) + "." + td.Name
			td.Ref = alias + "." + td.Name
		}
		typeByName[typeName] = td
	}

	return &imports{
		TypeByName:  typeByName,
		ImportLines: importLines,
	}, nil
}

type typeVisitor func(typeName string)

// visitTypesSCPD calls `visitor` with each data type name (e.g. "ui4") referenced
// by action arguments.`
func visitTypesSCPD(scpd *srvdesc.SCPD, visitor typeVisitor) error {
	for _, action := range scpd.ActionByName {
		if err := visitTypesAction(action, visitor); err != nil {
			return err
		}
	}
	return nil
}

func visitTypesAction(action *srvdesc.Action, visitor typeVisitor) error {
	for _, arg := range action.InArgs {
		sv, err := arg.RelatedStateVariable()
		if err != nil {
			return err
		}
		visitor(sv.DataType)
	}
	for _, arg := range action.OutArgs {
		sv, err := arg.RelatedStateVariable()
		if err != nil {
			return err
		}
		visitor(sv.DataType)
	}
	return nil
}
