package questions

import "testing"

func TestDefaultIsWellFormed(t *testing.T) {
	q := Default()
	if q.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", q.SchemaVersion, SchemaVersion)
	}

	ids := map[string]bool{}
	fields := map[string]bool{}
	for _, question := range q.Questions {
		if question.ID == "" || question.Field == "" || question.Prompt == "" {
			t.Errorf("question %+v is missing an id, field or prompt", question)
		}
		if ids[question.ID] {
			t.Errorf("duplicate question id %q", question.ID)
		}
		if fields[question.Field] {
			t.Errorf("duplicate field %q", question.Field)
		}
		ids[question.ID] = true
		fields[question.Field] = true

		switch question.Type {
		case TypeText:
			if len(question.Options) != 0 {
				t.Errorf("%s: text question has options", question.ID)
			}
		case TypeChoice:
			checkChoice(t, question)
		default:
			t.Errorf("%s: unknown type %q", question.ID, question.Type)
		}
	}
}

func checkChoice(t *testing.T, question Question) {
	t.Helper()
	if len(question.Options) < 2 {
		t.Errorf("%s: choice question needs at least two options", question.ID)
	}
	values := map[string]bool{}
	for _, o := range question.Options {
		if o.Value == "" || o.Label == "" {
			t.Errorf("%s: option %+v is missing a value or label", question.ID, o)
		}
		if o.Security == "" {
			t.Errorf("%s: option %q has no security note", question.ID, o.Value)
		}
		if values[o.Value] {
			t.Errorf("%s: duplicate option %q", question.ID, o.Value)
		}
		values[o.Value] = true

		// The default must never be an option that weakens the baseline.
		if o.Value == question.Default && o.RequiresAcknowledgement {
			t.Errorf("%s: default %q requires acknowledgement", question.ID, o.Value)
		}
	}
	if question.Default != "" && !values[question.Default] {
		t.Errorf("%s: default %q is not one of the options", question.ID, question.Default)
	}
}

func TestInClusterSecretsRequireAcknowledgement(t *testing.T) {
	for _, question := range Default().Questions {
		if question.ID != "secrets-backend" {
			continue
		}
		if question.Default != "" {
			t.Errorf("secrets-backend has default %q, want none", question.Default)
		}
		for _, o := range question.Options {
			if o.Value == "in-cluster" {
				if !o.RequiresAcknowledgement {
					t.Error("in-cluster secrets backend must require acknowledgement")
				}
				return
			}
		}
		t.Fatal("in-cluster option not found")
	}
	t.Fatal("secrets-backend question not found")
}
