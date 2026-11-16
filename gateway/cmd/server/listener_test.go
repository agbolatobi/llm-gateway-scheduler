package main

import "testing"

func TestListenerAddresses(testContext *testing.T) {
	for _, testCase := range []struct {
		address             string
		allow               bool
		configured, exposed string
		invalid             bool
	}{
		{"", false, "", "", false},
		{"127.0.0.1:8000", false, "127.0.0.1:8000", "", false},
		{"localhost:8000", true, "localhost:8000", "", false},
		{"0.0.0.0:8000", true, "127.0.0.1:8000", "0.0.0.0:8000", false},
		{"0.0.0.0:8000", false, "", "", true},
		{":8000", true, "", "", true},
		{"192.0.2.1:8000", true, "", "", true},
		{"0.0.0.0:0", true, "", "", true},
		{"localhost:65536", false, "", "", true},
	} {
		configured, exposed, err := listenerAddresses(testCase.address, testCase.allow)
		if (err != nil) != testCase.invalid || configured != testCase.configured || exposed != testCase.exposed {
			testContext.Fatalf("%q: configured=%q exposed=%q error=%v", testCase.address, configured, exposed, err)
		}
	}
}
