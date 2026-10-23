package module

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
)

// tsSource is the TypeScript twin, read from the repository, never copied:
// the contract checked here is the one the TypeScript package ships.
const tsSource = "../ts/src/engine-types.ts"

// twin maps one TypeScript member to the Go member it is the twin of. A Go
// type is named as reflect sees it; a Go member is a method of an interface
// or a field of a struct. An empty Go member records a TypeScript member
// with no Go twin, which the package documentation explains.
type twin struct{ goType, goMember string }

// twins is the table in the package documentation, member by member. Every
// exported TypeScript declaration and every member of it must have a row,
// and every row's Go side must exist: a member added on either side without
// the other fails here.
var twins = map[string]map[string]twin{
	"AdmittanceInstructions": {
		"outputsToAdmit": {"overlay.AdmittanceInstructions", "OutputsToAdmit"},
		"coinsToRetain":  {"overlay.AdmittanceInstructions", "CoinsToRetain"},
		"coinsRemoved":   {"overlay.AdmittanceInstructions", "CoinsRemoved"},
	},
	"TopicManager": {
		"identifyAdmissibleOutputs": {"TopicManager", "IdentifyAdmissibleOutputs"},
		"identifyNeededInputs":      {"InputIdentifier", "IdentifyNeededInputs"},
		"getDocumentation":          {"TopicManager", "GetDocumentation"},
		"getMetaData":               {"TopicManager", "GetMetaData"},
	},
	"OutputAdmittedByTopic": {
		"mode":           {"OutputAdmittedByTopic", "Mode"},
		"txid":           {"OutputAdmittedByTopic", "Outpoint"},
		"outputIndex":    {"OutputAdmittedByTopic", "Outpoint"},
		"topic":          {"OutputAdmittedByTopic", "Topic"},
		"satoshis":       {"OutputAdmittedByTopic", "Satoshis"},
		"lockingScript":  {"OutputAdmittedByTopic", "LockingScript"},
		"atomicBEEF":     {"OutputAdmittedByTopic", "AtomicBEEF"},
		"offChainValues": {"OutputAdmittedByTopic", "OffChainValues"},
	},
	"OutputSpent": {
		"mode":               {"OutputSpent", "Mode"},
		"txid":               {"OutputSpent", "Outpoint"},
		"outputIndex":        {"OutputSpent", "Outpoint"},
		"topic":              {"OutputSpent", "Topic"},
		"spendingTxid":       {"OutputSpent", "SpendingTxid"},
		"inputIndex":         {"OutputSpent", "InputIndex"},
		"unlockingScript":    {"OutputSpent", "UnlockingScript"},
		"sequenceNumber":     {"OutputSpent", "SequenceNumber"},
		"spendingAtomicBEEF": {"OutputSpent", "SpendingAtomicBEEF"},
		"offChainValues":     {"OutputSpent", "OffChainValues"},
	},
	"LookupFormula": {
		"txid":        {"lookup.LookupFormula", "Outpoint"},
		"outputIndex": {"lookup.LookupFormula", "Outpoint"},
		"history":     {"lookup.LookupFormula", "History"},
		"context":     {"", ""},
	},
	"LookupQuestion": {
		"service": {"lookup.LookupQuestion", "Service"},
		"query":   {"lookup.LookupQuestion", "Query"},
	},
	"LookupServiceMetaData": {
		"name":             {"overlay.MetaData", "Name"},
		"shortDescription": {"overlay.MetaData", "Description"},
		"iconURL":          {"overlay.MetaData", "Icon"},
		"version":          {"overlay.MetaData", "Version"},
		"informationURL":   {"overlay.MetaData", "InfoUrl"},
	},
	"LookupService": {
		"admissionMode":                   {"LookupService", "AdmissionMode"},
		"spendNotificationMode":           {"LookupService", "SpendNotificationMode"},
		"outputAdmittedByTopic":           {"LookupService", "OutputAdmittedByTopic"},
		"outputSpent":                     {"SpendListener", "OutputSpent"},
		"outputNoLongerRetainedInHistory": {"RetentionListener", "OutputNoLongerRetainedInHistory"},
		"outputEvicted":                   {"LookupService", "OutputEvicted"},
		"lookup":                          {"LookupService", "Lookup"},
		"getDocumentation":                {"LookupService", "GetDocumentation"},
		"getMetaData":                     {"LookupService", "GetMetaData"},
	},
	"Output": {
		"txid":            {"Output", "Outpoint"},
		"outputIndex":     {"Output", "Outpoint"},
		"outputScript":    {"Output", "OutputScript"},
		"satoshis":        {"Output", "Satoshis"},
		"topic":           {"Output", "Topic"},
		"spent":           {"Output", "Spent"},
		"outputsConsumed": {"Output", "OutputsConsumed"},
		"consumedBy":      {"Output", "ConsumedBy"},
		"beef":            {"Output", "Beef"},
		"blockHeight":     {"Output", "BlockHeight"},
		"score":           {"Output", "Score"},
	},
	"ModuleHost": {
		"log":     {"Host", "Log"},
		"metrics": {"Host", "Metrics"},
		"inc":     {"Metrics", "Inc"},
		"preset":  {"Metrics", "Preset"},
		"gauge":   {"Metrics", "Gauge"},
	},
	"RestoreStorage": {
		"findOutput": {"RestoreStorage", "FindOutput"},
	},
	"Module": {
		"topics":  {"Module", "Topics"},
		"lookups": {"Module", "Lookups"},
		"restore": {"Restorer", "Restore"},
	},
}

// goTypes are the Go types the table names, as reflect sees them.
var goTypes = map[string]reflect.Type{
	"overlay.AdmittanceInstructions": reflect.TypeOf(overlay.AdmittanceInstructions{}),
	"overlay.MetaData":               reflect.TypeOf(overlay.MetaData{}),
	"lookup.LookupFormula":           reflect.TypeOf(lookup.LookupFormula{}),
	"lookup.LookupQuestion":          reflect.TypeOf(lookup.LookupQuestion{}),
	"TopicManager":                   reflect.TypeOf((*TopicManager)(nil)).Elem(),
	"InputIdentifier":                reflect.TypeOf((*InputIdentifier)(nil)).Elem(),
	"OutputAdmittedByTopic":          reflect.TypeOf(OutputAdmittedByTopic{}),
	"OutputSpent":                    reflect.TypeOf(OutputSpent{}),
	"LookupService":                  reflect.TypeOf((*LookupService)(nil)).Elem(),
	"SpendListener":                  reflect.TypeOf((*SpendListener)(nil)).Elem(),
	"RetentionListener":              reflect.TypeOf((*RetentionListener)(nil)).Elem(),
	"Restorer":                       reflect.TypeOf((*Restorer)(nil)).Elem(),
	"Output":                         reflect.TypeOf(Output{}),
	"Host":                           reflect.TypeOf((*Host)(nil)).Elem(),
	"Metrics":                        reflect.TypeOf((*Metrics)(nil)).Elem(),
	"RestoreStorage":                 reflect.TypeOf((*RestoreStorage)(nil)).Elem(),
	"Module":                         reflect.TypeOf(Module{}),
}

var (
	tsDecl   = regexp.MustCompile(`^export (?:interface|type) (\w+)`)
	tsMember = regexp.MustCompile(`^\s*(?:readonly )?(\w+)\??:`)
	tsInline = regexp.MustCompile(`(?:\{|;)\s*(?:readonly )?(\w+)\??:`)
	// tsCase opens an object inline: a union case (| {) or an intersection
	// (& {). An object inside a member's parameters or return type does not.
	tsCase = regexp.MustCompile(`^\s*(?:\||.*&)\s*\{`)
)

// readTS reads the exported declarations and the member names of each from
// the TypeScript source. A member is a name followed by a colon at the
// start of a line, or after a brace or semicolon on a line that opens an
// object inline. A line inside a parenthesis or angle bracket opened on an
// earlier member line is that member's parameters or return type, not a
// member, except for an inline object it opens itself (Module's restore).
func readTS(t *testing.T) map[string][]string {
	t.Helper()
	src, err := os.ReadFile(tsSource)
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string][]string{}
	var cur string
	var parens, angles int
	seen := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		if m := tsDecl.FindStringSubmatch(line); m != nil {
			cur = m[1]
			decls[cur] = nil
			seen = map[string]bool{}
			parens, angles = 0, 0
			continue
		}
		if cur == "" || strings.HasPrefix(line, "}") || strings.HasPrefix(line, ">") {
			cur = ""
			continue
		}
		var names []string
		if parens == 0 && angles == 0 {
			if m := tsMember.FindStringSubmatch(line); m != nil {
				names = append(names, m[1])
			}
		}
		if parens == 0 && tsCase.MatchString(line) {
			for _, m := range tsInline.FindAllStringSubmatch(line, -1) {
				names = append(names, m[1])
			}
		}
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				decls[cur] = append(decls[cur], n)
			}
		}
		code := strings.ReplaceAll(line, "=>", "")
		parens += strings.Count(code, "(") - strings.Count(code, ")")
		angles += strings.Count(code, "<") - strings.Count(code, ">")
	}
	return decls
}

// TestTwinsCoverTheTypeScriptContract holds the table to the TypeScript
// source, both ways: every declaration and member there has a row, and no
// row names a member the source no longer has.
func TestTwinsCoverTheTypeScriptContract(t *testing.T) {
	decls := readTS(t)
	if len(decls) == 0 {
		t.Fatalf("no exported declarations read from %s", tsSource)
	}
	for name, members := range decls {
		rows, ok := twins[name]
		if !ok {
			t.Errorf("%s: declared in %s, no row in the table", name, tsSource)
			continue
		}
		for _, m := range members {
			if _, ok := rows[m]; !ok {
				t.Errorf("%s.%s: in %s, no row in the table", name, m, tsSource)
			}
		}
		for m := range rows {
			if !contains(members, m) {
				t.Errorf("%s.%s: in the table, not in %s", name, m, tsSource)
			}
		}
	}
	for name := range twins {
		if _, ok := decls[name]; !ok {
			t.Errorf("%s: in the table, not declared in %s", name, tsSource)
		}
	}
}

// TestTwinsExistInGo holds every row's Go side to the package and go-sdk:
// the type exists and has that method or field.
func TestTwinsExistInGo(t *testing.T) {
	for name, rows := range twins {
		for m, tw := range rows {
			if tw.goType == "" {
				continue
			}
			typ, ok := goTypes[tw.goType]
			if !ok {
				t.Errorf("%s.%s -> %s: Go type not listed", name, m, tw.goType)
				continue
			}
			var found bool
			switch typ.Kind() {
			case reflect.Interface:
				_, found = typ.MethodByName(tw.goMember)
			case reflect.Struct:
				_, found = typ.FieldByName(tw.goMember)
			}
			if !found {
				t.Errorf("%s.%s -> %s.%s: no such Go member", name, m, tw.goType, tw.goMember)
			}
		}
	}
}

// TestDocumentationListsEveryTwin holds the package documentation's table
// to the TypeScript source: every declaration is named in it.
func TestDocumentationListsEveryTwin(t *testing.T) {
	src, err := os.ReadFile("module.go")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(src[:strings.Index(string(src), "package module")])
	for name := range readTS(t) {
		if !strings.Contains(doc, name) {
			t.Errorf("%s: not in the package documentation", name)
		}
	}
}

// TestWireFieldNames holds the two payloads that cross the wire as JSON to
// the TypeScript member names: go-sdk's tags are what a TypeScript engine or
// client reads.
func TestWireFieldNames(t *testing.T) {
	decls := readTS(t)
	for name, v := range map[string]any{
		"AdmittanceInstructions": overlay.AdmittanceInstructions{OutputsToAdmit: []uint32{0}, CoinsToRetain: []uint32{1}, CoinsRemoved: []uint32{2}},
		"LookupServiceMetaData":  overlay.MetaData{Name: "n", Description: "d", Icon: "i", Version: "v", InfoUrl: "u"},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(b, &keys); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(keys))
		for k := range keys {
			got = append(got, k)
		}
		want := append([]string(nil), decls[name]...)
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: JSON fields %v, TypeScript members %v", name, got, want)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
