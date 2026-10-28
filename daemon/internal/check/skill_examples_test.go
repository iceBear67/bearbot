package check

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestSkillExamplesTypecheck runs the real type check (DESIGN 5.6 step 2) on every extension example of the bundled
// skills, so an example that would not pass bearbot_check fails here. The croner example needs a package that only
// the agent installs, so it is the one example that is skipped.
func TestSkillExamplesTypecheck(t *testing.T) {
	f := newFixture(t)
	f.requireSDK(t)
	skills, err := filepath.Glob(filepath.Join(f.env.Repo, "skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) == 0 {
		t.Fatalf("no skills in %s", filepath.Join(f.env.Repo, "skills"))
	}
	block := regexp.MustCompile("(?s)```ts\n(.*?)```")
	var croner []string
	written := 0
	for _, path := range skills {
		skill := filepath.Base(filepath.Dir(path))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		index := 0
		for _, m := range block.FindAllStringSubmatch(string(data), -1) {
			code := m[1]
			if !strings.Contains(code, "export default function") {
				continue
			}
			name := fmt.Sprintf("%s-%d.ts", skill, index)
			index++
			if strings.Contains(code, `from "croner"`) {
				croner = append(croner, skill)
				continue
			}
			if !strings.Contains(code, `from "/opt/bearbot/sdk/index.ts"`) {
				t.Fatalf("%s: example %s does not import the SDK from /opt/bearbot/sdk/index.ts", path, name)
			}
			f.ext(t, name, strings.ReplaceAll(code, `"/opt/bearbot/sdk/index.ts"`, strconv.Quote(f.env.Paths.SDKFile)))
			written++
		}
	}
	if !slices.Equal(croner, []string{"bearbot-middleware"}) {
		t.Fatalf("croner examples in %v, want only bearbot-middleware", croner)
	}
	if written != 8 {
		t.Fatalf("%d examples written, want 8", written)
	}
	tc, err := f.checker.Typecheck(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !tc.Ran || !tc.Passed || len(tc.Errors) != 0 {
		t.Fatalf("typecheck of the skill examples = %+v", tc)
	}
}
