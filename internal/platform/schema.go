package platform

import (
	"sort"
	"strings"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

// Schema returns a JSON Schema (draft 2020-12) for platform.yaml, generated
// from the questionnaire. It is for editors and agents; Validate is the
// authority, because it also checks acknowledgements and answers that cannot
// be combined, which the schema does not express.
func Schema(q questions.Questionnaire) map[string]any {
	root := object()
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["title"] = "platform.yaml"
	root["description"] = "Answers to the installation questionnaire. Holds references to secrets, never secret values."

	setProperty(root, keySchemaVersion, map[string]any{"const": q.SchemaVersion})
	require(root, keySchemaVersion)

	for _, question := range q.Questions {
		parts := strings.Split(question.Field, ".")
		node := root
		for _, part := range parts[:len(parts)-1] {
			props := node["properties"].(map[string]any)
			child, ok := props[part].(map[string]any)
			if !ok {
				child = object()
				props[part] = child
				require(node, part)
			}
			node = child
		}
		leaf := parts[len(parts)-1]
		setProperty(node, leaf, questionSchema(question))
		require(node, leaf)
	}

	ack := object()
	for _, key := range []string{"question", "option", "reason"} {
		setProperty(ack, key, map[string]any{"type": "string", "minLength": 1})
		require(ack, key)
	}
	setProperty(root, keyAcknowledgements, map[string]any{
		"type":        "array",
		"description": "One entry for each chosen option that weakens the security baseline.",
		"items":       ack,
	})
	return root
}

func object() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{},
		"required":             []string{},
	}
}

func setProperty(node map[string]any, name string, schema map[string]any) {
	node["properties"].(map[string]any)[name] = schema
}

func require(node map[string]any, name string) {
	required := append(node["required"].([]string), name)
	sort.Strings(required)
	node["required"] = required
}

func questionSchema(question questions.Question) map[string]any {
	s := map[string]any{"description": question.Prompt}
	if question.Type == questions.TypeText {
		s["type"] = "string"
		s["minLength"] = 1
		if question.Format == questions.FormatHTTPSURL {
			s["pattern"] = "^https://"
		}
		return s
	}
	options := make([]map[string]any, 0, len(question.Options))
	for _, o := range question.Options {
		options = append(options, map[string]any{
			"const":       o.Value,
			"title":       o.Label,
			"description": o.Security,
		})
	}
	s["oneOf"] = options
	return s
}
