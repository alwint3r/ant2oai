package main

import (
	"strings"
	"testing"
)

func TestCheckJSONDepth(t *testing.T) {
	const tooDeep = "JSON nesting exceeds 128 levels"
	deepBrackets := strings.Repeat("[", 200)

	cases := []struct {
		name string
		data string
		want string
	}{
		{name: "empty", data: ""},
		{name: "scalar", data: "1"},
		{
			name: "at the limit",
			data: strings.Repeat("[", maxJSONDepth) + strings.Repeat("]", maxJSONDepth),
		},
		{
			name: "over the limit",
			data: strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1),
			want: tooDeep,
		},
		{
			name: "objects count as levels",
			data: strings.Repeat(`{"a":`, maxJSONDepth+1), want: tooDeep,
		},
		{
			name: "closing brackets restore the depth",
			data: strings.Repeat(strings.Repeat("[", 100)+strings.Repeat("]", 100), 3),
		},
		{name: "brackets inside a string", data: `"` + deepBrackets + `"`},
		{name: "escaped quote stays inside a string", data: `"\"` + deepBrackets + `"`},
		{
			name: "escaped backslash ends with the string",
			data: `"\\\\"` + deepBrackets, want: tooDeep,
		},
		{name: "invalid UTF-8", data: "\"\xff\"", want: "JSON must use valid UTF-8"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := checkJSONDepth([]byte(testCase.data))
			if testCase.want != "" {
				assertError(t, err, testCase.want)
				return
			}
			if err != nil {
				t.Errorf("checkJSONDepth: %v", err)
			}
		})
	}
}
