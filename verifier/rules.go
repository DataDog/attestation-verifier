package verifier

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"strings"

	"github.com/google/cel-go/cel"
	linkPredicatev0 "github.com/in-toto/attestation/go/predicates/link/v0"
	provenancePredicatev1 "github.com/in-toto/attestation/go/predicates/provenance/v1"
	attestationv1 "github.com/in-toto/attestation/go/v1"
	"github.com/in-toto/in-toto-golang/in_toto"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protojson"
)

func applyArtifactRules(statement *attestationv1.Statement, materialRules []string, productRules []string, claims map[string]map[AttestationIdentifier]*attestationv1.Statement) error {
	materialsList, productsList, err := getMaterialsAndProducts(statement)
	if err != nil {
		return err
	}

	materials := map[string]*attestationv1.ResourceDescriptor{}
	materialsPaths := in_toto.NewSet()
	for _, artifact := range materialsList {
		artifact := artifact
		materials[artifact.Name] = artifact
		materialsPaths.Add(path.Clean(artifact.Name))
	}

	products := map[string]*attestationv1.ResourceDescriptor{}
	productsPaths := in_toto.NewSet()
	for _, artifact := range productsList {
		artifact := artifact
		products[artifact.Name] = artifact
		productsPaths.Add(path.Clean(artifact.Name))
	}

	created := productsPaths.Difference(materialsPaths)
	deleted := materialsPaths.Difference(productsPaths)
	remained := materialsPaths.Intersection(productsPaths)
	modified := in_toto.NewSet()
	for name := range remained {
		if !reflect.DeepEqual(materials[name].Digest, products[name].Digest) {
			modified.Add(name)
		}
	}

	log.Infof("Applying material rules...")
	for _, r := range materialRules {
		log.Infof("Evaluating rule `%s`...", r)
		rule, err := in_toto.UnpackRule(strings.Split(r, " "))
		if err != nil {
			return err
		}

		filtered := materialsPaths.Filter(path.Clean(rule["pattern"]))
		var consumed in_toto.Set
		switch rule["type"] {
		case "match":
			consumed = applyMatchRule(rule, materials, materialsPaths, claims)
		case "allow":
			consumed = filtered
		case "delete":
			consumed = filtered.Intersection(deleted)
		case "disallow":
			if len(filtered) > 0 {
				return fmt.Errorf("materials verification failed: %s disallowed by rule %s", filtered.Slice(), rule)
			}
		case "require":
			if !materialsPaths.Has(rule["pattern"]) {
				return fmt.Errorf("materials verification failed: %s required but not found", rule["pattern"])
			}
		default:
			return fmt.Errorf("invalid material rule %s", rule["type"])
		}
		materialsPaths = materialsPaths.Difference(consumed)
	}

	// I've separated these out on purpose right now
	log.Infof("Applying product rules...")
	for _, r := range productRules {
		log.Infof("Evaluating rule `%s`...", r)
		rule, err := in_toto.UnpackRule(strings.Split(r, " "))
		if err != nil {
			return err
		}

		filtered := productsPaths.Filter(path.Clean(rule["pattern"]))
		var consumed in_toto.Set
		switch rule["type"] {
		case "match":
			consumed = applyMatchRule(rule, products, productsPaths, claims)
		case "allow":
			consumed = filtered
		case "create":
			consumed = filtered.Intersection(created)
		case "modify":
			consumed = filtered.Intersection(modified)
		case "disallow":
			if len(filtered) > 0 {
				return fmt.Errorf("products verification failed: %s disallowed by rule %s", filtered.Slice(), rule)
			}
		case "require":
			if !productsPaths.Has(rule["pattern"]) {
				return fmt.Errorf("products verification failed: %s required but not found", rule["pattern"])
			}
		default:
			return fmt.Errorf("invalid product rule %s", rule["type"])
		}
		productsPaths = productsPaths.Difference(consumed)
	}

	return nil
}

func applyAttributeRules(predicateType string, predicate map[string]any, rules []Constraint) error {
	env, err := getCELEnvForPredicateType(predicateType)
	if err != nil {
		return err
	}

	log.Infof("Applying attribute rules...")
	for _, r := range rules {
		log.Infof("Evaluating rule `%s`...", r.Rule)
		ast, issues := env.Compile(r.Rule)
		if issues != nil && issues.Err() != nil {
			return issues.Err()
		}

		prog, err := env.Program(ast)
		if err != nil {
			return err
		}

		out, _, err := prog.Eval(predicate)
		if err != nil {
			if strings.Contains(err.Error(), "no such attribute") && r.AllowIfNoClaim {
				continue
			}
		}
		if result, ok := out.Value().(bool); !ok {
			return fmt.Errorf("unexpected result from CEL")
		} else if !result {
			return fmt.Errorf("verification failed for rule '%s'", r.Rule)
		}
	}

	return nil
}

func getMaterialsAndProducts(statement *attestationv1.Statement) ([]*attestationv1.ResourceDescriptor, []*attestationv1.ResourceDescriptor, error) {
	switch statement.PredicateType {
	case "https://in-toto.io/attestation/link/v0.3":
		linkBytes, err := json.Marshal(statement.Predicate)
		if err != nil {
			return nil, nil, err
		}

		link := &linkPredicatev0.Link{}
		if err := protojson.Unmarshal(linkBytes, link); err != nil {
			return nil, nil, err
		}

		return link.Materials, statement.Subject, nil

	case "https://slsa.dev/provenance/v1":
		provenanceBytes, err := json.Marshal(statement.Predicate)
		if err != nil {
			return nil, nil, err
		}

		provenance := &provenancePredicatev1.Provenance{}
		if err := protojson.Unmarshal(provenanceBytes, provenance); err != nil {
			return nil, nil, err
		}

		return provenance.BuildDefinition.ResolvedDependencies, statement.Subject, nil

	default:
		return statement.Subject, nil, nil
	}
}

func applyMatchRule(rule map[string]string, srcArtifacts map[string]*attestationv1.ResourceDescriptor, queue in_toto.Set, claims map[string]map[AttestationIdentifier]*attestationv1.Statement) in_toto.Set {
	consumed := in_toto.NewSet()

	dstClaims, ok := claims[rule["dstName"]]
	if !ok {
		return consumed
	}

	dstMaterials, dstProducts, err := getDestinationArtifacts(dstClaims)
	if err != nil {
		// FIXME: what is the right behaviour here across claims?
		return consumed
	}

	var dstArtifacts map[string]*attestationv1.ResourceDescriptor
	if rule["dstType"] == "materials" {
		dstArtifacts = dstMaterials
	} else {
		dstArtifacts = dstProducts
	}

	if rule["pattern"] != "" {
		rule["pattern"] = path.Clean(rule["pattern"])
	}

	for p := range srcArtifacts {
		if path.Clean(p) != p {
			srcArtifacts[path.Clean(p)] = srcArtifacts[p]
			delete(srcArtifacts, p)
		}
	}

	for p := range dstArtifacts {
		if path.Clean(p) != p {
			dstArtifacts[path.Clean(p)] = dstArtifacts[p]
			delete(dstArtifacts, p)
		}
	}

	for _, prefix := range []string{"srcPrefix", "dstPrefix"} {
		if rule[prefix] != "" {
			rule[prefix] = path.Clean(rule[prefix])
			if !strings.HasSuffix(rule[prefix], "/") {
				rule[prefix] += "/"
			}
		}
	}

	for srcPath := range queue {
		srcBasePath := strings.TrimPrefix(srcPath, rule["srcPrefix"])

		// Ignore artifacts not matched by rule pattern
		matched, err := match(rule["pattern"], srcBasePath)
		if err != nil || !matched {
			continue
		}

		// Construct corresponding destination artifact path, i.e.
		// an optional destination prefix plus the source base path
		dstPath := path.Clean(path.Join(rule["dstPrefix"], srcBasePath))

		// Try to find the corresponding destination artifact
		dstArtifact, exists := dstArtifacts[dstPath]
		// Ignore artifacts without corresponding destination artifact
		if !exists {
			continue
		}

		// Ignore artifact pairs with no matching hashes
		if !reflect.DeepEqual(srcArtifacts[srcPath].Digest, dstArtifact.Digest) {
			continue
		}

		// Only if a source and destination artifact pair was found and
		// their hashes are equal, will we mark the source artifact as
		// successfully consumed, i.e. it will be removed from the queue
		consumed.Add(srcPath)
	}

	return consumed
}

func getDestinationArtifacts(dstClaims map[AttestationIdentifier]*attestationv1.Statement) (map[string]*attestationv1.ResourceDescriptor, map[string]*attestationv1.ResourceDescriptor, error) {
	materials := map[string]*attestationv1.ResourceDescriptor{}
	products := map[string]*attestationv1.ResourceDescriptor{}

	for _, claim := range dstClaims {
		materialsList, productsList, err := getMaterialsAndProducts(claim)
		if err != nil {
			return nil, nil, err
		}

		// FIXME: we're overwriting artifact info without checking if claims agree

		for _, artifact := range materialsList {
			artifact := artifact
			materials[artifact.Name] = artifact
		}

		for _, artifact := range productsList {
			artifact := artifact
			products[artifact.Name] = artifact
		}
	}

	return materials, products, nil
}

func getCELEnvForPredicateType(predicateType string) (*cel.Env, error) {
	// FIXME: maybe we should take over https://github.com/google/cel-go/pull/219

	switch predicateType {
	case "https://in-toto.io/attestation/link/v0.3":
		return cel.NewEnv(
			cel.Variable("name", cel.StringType),
			cel.Variable("command", cel.ListType(cel.StringType)),
			cel.Variable("materials", cel.ListType(cel.ObjectType("in_toto_attestation.v1.ResourceDescriptor"))),
			cel.Variable("byproducts", cel.ObjectType("google.protobuf.Struct")),
			cel.Variable("environment", cel.ObjectType("google.protobuf.Struct")),
		)
	case "https://in-toto.io/attestation/test-result/v0.1":
		return cel.NewEnv(
			cel.Variable("result", cel.StringType),
			cel.Variable("configuration", cel.ListType(cel.ObjectType("in_toto_attestation.v1.ResourceDescriptor"))),
			cel.Variable("passedTests", cel.ListType(cel.StringType)),
			cel.Variable("warnedTests", cel.ListType(cel.StringType)),
			cel.Variable("failedTests", cel.ListType(cel.StringType)),
		)
	case "https://slsa.dev/provenance/v1":
		return cel.NewEnv(
			cel.Variable("buildDefinition", cel.ObjectType("in_toto_attestation.predicates.provenance.v1.BuildDefinition")),
			cel.Variable("runDetails", cel.ObjectType("in_toto_attestation.predicates.provenance.v1.RunDetails")),
		)
	}

	return nil, fmt.Errorf("unknown predicate type")
}
