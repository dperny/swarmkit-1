package signablespec

import (
	"github.com/gogo/protobuf/gogoproto"
	"github.com/gogo/protobuf/protoc-gen-gogo/generator"

	"strings"
)

const SWARM_SIGNABLE_TYPE = ".docker.swarmkit.v1.SpecSignature"

type signableSpecGen struct {
	gen *generator.Generator
	generator.PluginImports
	protoPkg generator.Single
	// signables is a list of objects we've found to be signable (have a
	// signature field)
	signables map[string]*generator.Descriptor
}

func (d *signableSpecGen) Name() string {
	return "signablespec"
}

func (d *signableSpecGen) Init(g *generator.Generator) {
	d.gen = g
}

func (d *signableSpecGen) genSignableInterface() {
	d.gen.P("type SignableSpec interface {")
	d.gen.In()
	// embed proto.Message so we can use signable
	d.gen.P(d.protoPkg.Use(), ".Message")
	d.gen.P("GetSignature() *SpecSignature")
	d.gen.P("SetSignature(*SpecSignature)")
	d.gen.P("GetSubspecs() []SignableSpec")
	d.gen.Out()
	d.gen.P("}")
}

func (d *signableSpecGen) genGetSignature(ccTypeName string, m *generator.Descriptor) {
	d.gen.P("func (s *", ccTypeName, ") GetSignature() *SpecSignature {")
	d.gen.In()
	d.gen.P("if s == nil {")
	d.gen.In()
	d.gen.P("return nil")
	d.gen.Out()
	d.gen.P("}")
	d.gen.P("return s.Signature")
	d.gen.Out()
	d.gen.P("}")
}

func (d *signableSpecGen) genSetSignature(ccTypeName string, m *generator.Descriptor) {
	d.gen.P("func (s *", ccTypeName, ") SetSignature(sig *SpecSignature) {")
	d.gen.Out()
	d.gen.P("if s == nil {")
	d.gen.Out()
	d.gen.P("return")
	d.gen.In()
	d.gen.P("}")
	d.gen.P("s.Signature = sig")
	d.gen.In()
	d.gen.P("}")
}

func (d *signableSpecGen) genGetSubspecs(ccTypeName string, m *generator.Descriptor) {
	d.gen.P("func (s *", ccTypeName, ") GetSubspecs() []SignableSpec {")
	d.gen.Out()
	d.gen.P("if s == nil {")
	d.gen.Out()
	d.gen.P("return []SignableSpec{}")
	d.gen.In()
	d.gen.P("}")
	// d.gen.P("return []SignableSpec{")
	d.gen.Out()
	// check for other fields that are specs
	d.gen.P("subspecs := []SignableSpec{}")
	if ccTypeName == "TaskSpec" {
		d.gen.P("var runtime SignableSpec")
		d.gen.P("switch rs := s.Runtime.(type) {")
		d.gen.P("case *TaskSpec_Attachment:")
		d.gen.Out()
		d.gen.P("runtime = rs.Attachment")
		d.gen.In()
		d.gen.P("case *TaskSpec_Container:")
		d.gen.Out()
		d.gen.P("runtime = rs.Container")
		d.gen.In()
		d.gen.P("case *TaskSpec_Generic:")
		d.gen.Out()
		d.gen.P("runtime = rs.Generic")
		d.gen.In()
		d.gen.P("}")
		d.gen.P("subspecs = append(subspecs, runtime)")
	}
	for _, f := range m.Field {
		// skip anything that doesn't have a name. not sure how that happens.
		if f.Name == nil {
			continue
		}
		if tn := f.TypeName; tn != nil {
			subfieldType := generator.CamelCase(*tn)
			// de-fully-qualify the type name
			components := strings.Split(subfieldType, ".")
			if len(components) != 0 {
				subfieldType = components[len(components)-1]
			}
			if _, ok := d.signables[subfieldType]; ok {
				// if this is the first subspec we find, then start the array
				n := generator.CamelCase(*f.Name)
				// TODO(dperny): i hate myself and i hate this
				if ccTypeName == "TaskSpec" {
					switch n {
					case "Container", "Attachment", "Generic":
						continue
					default:
						// keep going, not a runtime
					}
				}

				if !gogoproto.IsNullable(f) {
					// if the field is not nullable, return a pointer to it
					d.gen.P("subspecs = append(subspecs, &(s.", generator.CamelCase(*f.Name), "))")
				} else {
					d.gen.P("if ss := s.", generator.CamelCase(*f.Name), "; ss != nil {")
					d.gen.Out()
					d.gen.P("subspecs = append(subspecs, ss)")
					d.gen.In()
					d.gen.P("}")
				}
			}
		}
	}
	d.gen.P("return subspecs")
	d.gen.In()
	d.gen.P("}")
}

func (d *signableSpecGen) Generate(file *generator.FileDescriptor) {
	d.PluginImports = generator.NewPluginImports(d.gen)
	// import golang proto
	d.protoPkg = d.NewImport("github.com/gogo/protobuf/proto")
	// first, go through the messages, and find the signable ones
	d.signables = map[string]*generator.Descriptor{}
	// go for every message type
	for _, m := range file.Messages() {
		// check if the message has a SpecSignature message embedded. If not,
		// skip it
		signable := false
		ccTypeName := generator.CamelCaseSlice(m.TypeName())
		for _, f := range m.GetField() {
			if f.TypeName != nil {
				ccFieldType := generator.CamelCase(*f.TypeName)
				if ccFieldType == SWARM_SIGNABLE_TYPE {
					signable = true
					break
				}
			}
		}
		if signable {
			d.signables[ccTypeName] = m
		}
	}
	if len(d.signables) > 0 {
		d.genSignableInterface()
	}

	for ccTypeName, m := range d.signables {
		d.genGetSignature(ccTypeName, m)
		d.genSetSignature(ccTypeName, m)
		d.genGetSubspecs(ccTypeName, m)
		d.gen.P()
	}
}

func init() {
	generator.RegisterPlugin(new(signableSpecGen))
}
