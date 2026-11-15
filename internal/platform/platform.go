// Package platform validates platform.yaml, the file that records the
// answers to the installation questionnaire.
//
// The questionnaire is the single source of truth: the set of fields, the
// allowed values and the options that need an acknowledgement all come from
// it, so the file format cannot drift from the questions.
package platform

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/questions"
)

// Top-level keys that are not answers to questions.
const (
	keySchemaVersion    = "schemaVersion"
	keyAcknowledgements = "acknowledgements"
)

// Problem codes. They are stable and safe to branch on.
const (
	CodeMissingAnswer          = "missing_answer"
	CodeInvalidValue           = "invalid_value"
	CodeWrongType              = "wrong_type"
	CodeUnknownField           = "unknown_field"
	CodeUnsupportedVersion     = "unsupported_schema_version"
	CodeMissingAcknowledgement = "missing_acknowledgement"
	CodeUnusedAcknowledgement  = "unused_acknowledgement"
	CodeIncompatibleAnswers    = "incompatible_answers"
)

// Problem is one thing wrong with the file.
type Problem struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Deviation is an answer that weakens the security baseline and has been
// acknowledged in the file.
type Deviation struct {
	Question string `json:"question"`
	Field    string `json:"field"`
	Option   string `json:"option"`
	Security string `json:"security"`
	Reason   string `json:"reason"`
}

// Result is the outcome of validating one file.
type Result struct {
	Valid    bool      `json:"valid"`
	Problems []Problem `json:"problems"`
	// Deviations lists the acknowledged departures from the secure defaults.
	// It is only meaningful when Valid is true.
	Deviations []Deviation `json:"deviations"`
	// Answers maps each question's field to its answer. It is only complete
	// when Valid is true.
	Answers map[string]string `json:"-"`
}

type acknowledgement struct {
	question, option, reason string
	used                     bool
}

type validator struct {
	q        questions.Questionnaire
	byField  map[string]questions.Question
	problems []Problem
}

// Validate checks data against the questionnaire. It returns an error only
// when data cannot be read as a single YAML document; everything else is
// reported as problems in the result.
func Validate(data []byte, q questions.Questionnaire) (Result, error) {
	doc, err := parse(data)
	if err != nil {
		return Result{}, err
	}

	v := &validator{q: q, byField: map[string]questions.Question{}}
	for _, question := range q.Questions {
		v.byField[question.Field] = question
	}

	v.checkSchemaVersion(doc)
	answers := v.collectAnswers(doc)
	acks := v.collectAcknowledgements(doc)
	v.checkUnknownFields(doc, "")
	deviations := v.checkAnswers(answers, acks)
	v.checkCompatibility(answers)

	for i, a := range acks {
		if !a.used {
			v.add(fmt.Sprintf("%s[%d]", keyAcknowledgements, i), CodeUnusedAcknowledgement,
				fmt.Sprintf("acknowledgement for %q option %q does not match a chosen answer", a.question, a.option),
				"remove it, or check the question id and option value")
		}
	}

	res := Result{
		Valid:      len(v.problems) == 0,
		Problems:   v.problems,
		Deviations: deviations,
		Answers:    answers,
	}
	if res.Problems == nil {
		res.Problems = []Problem{}
	}
	if res.Deviations == nil || !res.Valid {
		res.Deviations = []Deviation{}
	}
	return res, nil
}

func parse(data []byte) (map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root any
	if err := dec.Decode(&root); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("the file must contain exactly one YAML document")
	}
	if root == nil {
		return map[string]any{}, nil
	}
	doc, ok := root.(map[string]any)
	if !ok {
		return nil, errors.New("the top level of the file must be a mapping")
	}
	return doc, nil
}

func (v *validator) add(field, code, message, hint string) {
	v.problems = append(v.problems, Problem{Field: field, Code: code, Message: message, Hint: hint})
}

func (v *validator) checkSchemaVersion(doc map[string]any) {
	raw, ok := doc[keySchemaVersion]
	if !ok {
		v.add(keySchemaVersion, CodeMissingAnswer, "schemaVersion is required",
			fmt.Sprintf("add `schemaVersion: %s`", v.q.SchemaVersion))
		return
	}
	if s, _ := raw.(string); s != v.q.SchemaVersion {
		v.add(keySchemaVersion, CodeUnsupportedVersion,
			fmt.Sprintf("schemaVersion is %v, but this version of the tool reads %s", raw, v.q.SchemaVersion), "")
	}
}

// lookup returns the value at a dotted path, and whether it is present.
func lookup(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[key]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// collectAnswers returns the string answer for every question that has one,
// reporting answers that are missing or not strings.
func (v *validator) collectAnswers(doc map[string]any) map[string]string {
	answers := map[string]string{}
	for _, question := range v.q.Questions {
		raw, ok := lookup(doc, question.Field)
		if !ok || raw == nil {
			hint := ""
			if question.Default != "" {
				hint = fmt.Sprintf("the secure default is %q; answers must be written out explicitly", question.Default)
			}
			v.add(question.Field, CodeMissingAnswer,
				fmt.Sprintf("no answer for question %q", question.ID), hint)
			continue
		}
		s, ok := raw.(string)
		if !ok {
			v.add(question.Field, CodeWrongType,
				fmt.Sprintf("expected a string, got %s", describe(raw)),
				"quote the value so YAML reads it as text")
			continue
		}
		answers[question.Field] = s
	}
	return answers
}

func describe(raw any) string {
	switch raw.(type) {
	case bool:
		return "a boolean"
	case int, int64, uint64, float64:
		return "a number"
	case map[string]any:
		return "a mapping"
	case []any:
		return "a list"
	default:
		return fmt.Sprintf("%T", raw)
	}
}

func (v *validator) collectAcknowledgements(doc map[string]any) []*acknowledgement {
	raw, ok := doc[keyAcknowledgements]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		v.add(keyAcknowledgements, CodeWrongType, "acknowledgements must be a list", "")
		return nil
	}
	var acks []*acknowledgement
	for i, item := range list {
		path := fmt.Sprintf("%s[%d]", keyAcknowledgements, i)
		m, ok := item.(map[string]any)
		if !ok {
			v.add(path, CodeWrongType, "an acknowledgement must be a mapping with question, option and reason", "")
			continue
		}
		a := &acknowledgement{}
		complete := true
		for _, f := range []struct {
			key string
			dst *string
		}{{"question", &a.question}, {"option", &a.option}, {"reason", &a.reason}} {
			s, _ := m[f.key].(string)
			if strings.TrimSpace(s) == "" {
				v.add(path+"."+f.key, CodeMissingAnswer, f.key+" is required and must be text", "")
				complete = false
			}
			*f.dst = s
		}
		for _, key := range sortedKeys(m) {
			if key != "question" && key != "option" && key != "reason" {
				v.add(path+"."+key, CodeUnknownField, "unknown field", "")
			}
		}
		if complete {
			acks = append(acks, a)
		}
	}
	return acks
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkUnknownFields reports every key that is not part of a question's field
// path. Typos in a security setting must not be silently ignored.
func (v *validator) checkUnknownFields(m map[string]any, prefix string) {
	for _, key := range sortedKeys(m) {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if prefix == "" && (key == keySchemaVersion || key == keyAcknowledgements) {
			continue
		}
		if _, ok := v.byField[path]; ok {
			continue
		}
		if !v.isFieldPrefix(path) {
			v.add(path, CodeUnknownField, "unknown field", "")
			continue
		}
		child, ok := m[key].(map[string]any)
		if !ok {
			if m[key] != nil {
				v.add(path, CodeWrongType, fmt.Sprintf("expected a mapping, got %s", describe(m[key])), "")
			}
			continue
		}
		v.checkUnknownFields(child, path)
	}
}

func (v *validator) isFieldPrefix(path string) bool {
	for field := range v.byField {
		if strings.HasPrefix(field, path+".") {
			return true
		}
	}
	return false
}

func (v *validator) checkAnswers(answers map[string]string, acks []*acknowledgement) []Deviation {
	var deviations []Deviation
	for _, question := range v.q.Questions {
		answer, ok := answers[question.Field]
		if !ok {
			continue
		}
		if question.Type == questions.TypeText {
			v.checkText(question, answer)
			continue
		}

		var chosen *questions.Option
		values := make([]string, 0, len(question.Options))
		for i, o := range question.Options {
			values = append(values, o.Value)
			if o.Value == answer {
				chosen = &question.Options[i]
			}
		}
		if chosen == nil {
			v.add(question.Field, CodeInvalidValue,
				fmt.Sprintf("%q is not an option for question %q", answer, question.ID),
				"allowed values: "+strings.Join(values, ", "))
			continue
		}
		if !chosen.RequiresAcknowledgement {
			continue
		}

		var match *acknowledgement
		for _, a := range acks {
			if a.question == question.ID && a.option == chosen.Value {
				a.used = true
				if match == nil {
					match = a
				}
			}
		}
		if match == nil {
			v.add(question.Field, CodeMissingAcknowledgement,
				fmt.Sprintf("%q weakens the security baseline and has not been acknowledged: %s", chosen.Value, chosen.Security),
				fmt.Sprintf("add an entry to acknowledgements with question %q, option %q and a reason", question.ID, chosen.Value))
			continue
		}
		deviations = append(deviations, Deviation{
			Question: question.ID,
			Field:    question.Field,
			Option:   chosen.Value,
			Security: chosen.Security,
			Reason:   match.reason,
		})
	}
	return deviations
}

func (v *validator) checkText(question questions.Question, answer string) {
	if strings.TrimSpace(answer) == "" {
		v.add(question.Field, CodeMissingAnswer, fmt.Sprintf("no answer for question %q", question.ID), "")
		return
	}
	if question.Format == questions.FormatHTTPSURL {
		u, err := url.Parse(answer)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			v.add(question.Field, CodeInvalidValue,
				fmt.Sprintf("%q is not an https:// URL", answer),
				"use the full issuer URL, for example https://accounts.example.com")
		}
	}
}

// checkCompatibility reports answers that are each valid but cannot be
// combined.
func (v *validator) checkCompatibility(answers map[string]string) {
	host, hasHost := answers["git.host"]
	if answers["git.credential"] == "github-app" && hasHost && host != "github" {
		v.add("git.credential", CodeIncompatibleAnswers,
			fmt.Sprintf("github-app credentials only work when git.host is github, but it is %q", host),
			"use deploy-key for this Git host")
	}
}
