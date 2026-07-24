// Copyright 2024 Gelotto
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package procscan

import "testing"

func TestParseStatData(t *testing.T) {
	// comm containing spaces and parentheses must not break field offsets
	stat := "55648 (my (weird) comm) S 55565 55648 55565 34816 55648 4194304 " +
		"100 0 0 0 5 3 0 0 20 0 8 0 123456 1000000 500 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"

	ppid, starttime, err := parseStatData(stat)
	if err != nil {
		t.Fatalf("parseStatData: %v", err)
	}
	if ppid != 55565 {
		t.Errorf("ppid = %d, want 55565", ppid)
	}
	if starttime != 123456 {
		t.Errorf("starttime = %d, want 123456", starttime)
	}

	if _, _, err := parseStatData("garbage with no paren"); err == nil {
		t.Error("expected error for malformed stat")
	}
}
