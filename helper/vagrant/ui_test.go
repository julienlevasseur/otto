package vagrant

import (
	"reflect"
	"testing"
)

func TestVagrantUi(t *testing.T) {
	cases := map[string]struct {
		Types    []string
		Messages []string
		Outputs  []*Output
	}{
		"basic": {
			[]string{"ui"},
			[]string{"1376289459,,ui,say,foo bar"},
			[]*Output{
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo bar",
					},
				},
			},
		},

		"multiple": {
			[]string{"ui"},
			[]string{
				"1376289459,,ui,say,foo bar",
				"\n",
				"1376289459,,ui,say,baz",
			},
			[]*Output{
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo bar",
					},
				},
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"baz",
					},
				},
			},
		},

		"multiple in one": {
			[]string{"ui"},
			[]string{
				"1376289459,,ui,say,foo bar\n1376289459,,ui,say,baz\n1376289459,,ui,say,qux",
			},
			[]*Output{
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo bar",
					},
				},
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"baz",
					},
				},
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"qux",
					},
				},
			},
		},

		"trailing newline": {
			[]string{"ui"},
			[]string{
				"1376289459,,ui,say,foo bar",
				"\n",
				"1376289459,,ui,say,baz\n",
			},
			[]*Output{
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo bar",
					},
				},
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"baz",
					},
				},
			},
		},

		"newlines in data": {
			[]string{"ui"},
			[]string{"1376289459,,ui,say,foo\\nbar"},
			[]*Output{
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo\nbar",
					},
				},
			},
		},

		"carriage return in data": {
			[]string{"ui"},
			[]string{"1376289459,,ui,say,foo\\rbar"},
			[]*Output{
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo\rbar",
					},
				},
			},
		},

		"comma in data": {
			[]string{"ui"},
			[]string{"1376289459,,ui,say,foo%!(VAGRANT_COMMA)bar"},
			[]*Output{
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo,bar",
					},
				},
			},
		},

		"invalid line": {
			[]string{"invalid", "ui"},
			[]string{"foo\n1376289459,,ui,say,foo%!(VAGRANT_COMMA)bar"},
			[]*Output{
				&Output{
					Timestamp: "",
					Target:    "",
					Type:      "invalid",
					Data: []string{
						"foo",
					},
				},
				&Output{
					Timestamp: "1376289459",
					Target:    "",
					Type:      "ui",
					Data: []string{
						"say",
						"foo,bar",
					},
				},
			},
		},

		"unregistered type": {
			[]string{"ui"},
			[]string{"1376289459,,not-ui,say,foo bar"},
			[]*Output{},
		},
	}

	for name, tc := range cases {
		actual := make([]*Output, 0, len(tc.Outputs))
		recordCallback := func(o *Output) {
			actual = append(actual, o)
		}

		callbacks := make(map[string]OutputCallback)
		for _, t := range tc.Types {
			callbacks[t] = recordCallback
		}

		ui := &vagrantUi{Callbacks: callbacks}
		for _, msg := range tc.Messages {
			ui.Raw(msg)
		}
		ui.Finish()

		if !reflect.DeepEqual(actual, tc.Outputs) {
			t.Fatalf("%s\n\n%#v\n\n%#v", name, actual, tc.Outputs)
		}
	}
}
