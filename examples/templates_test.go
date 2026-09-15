package examples

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A markdown link's or image's target — `](target` — and a reference
// definition's — `[name]: target`.
var (
	inlineLink = regexp.MustCompile(`\]\(([^)\s]+)`)
	refLink    = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]:\s*(\S+)`)
)

// Each release publishes examples/node and examples/deno as template
// repositories of their own (release.yml's templates job), and every
// repository made from one is its own root too: a relative link that
// leaves the example's directory works here and is a 404 there. So a
// link in an example's docs is absolute, or stays inside the example
// and names something that exists.
func TestTemplateLinksStayInside(t *testing.T) {
	for _, ex := range []string{"node", "deno"} {
		err := filepath.WalkDir(ex, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var targets []string
			for _, re := range []*regexp.Regexp{inlineLink, refLink} {
				for _, m := range re.FindAllStringSubmatch(string(b), -1) {
					targets = append(targets, m[1])
				}
			}
			for _, target := range targets {
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
					continue
				}
				file, _, _ := strings.Cut(target, "#")
				resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(file))
				rel, err := filepath.Rel(ex, resolved)
				// A leading / is the repository's root, hotserve's here
				// and the example's own in the template: it cannot name
				// the same file in both.
				if strings.HasPrefix(file, "/") || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					t.Errorf("%s links to %s, outside examples/%s: in its template repository that is a 404; link to https://github.com/smallhoursorg/hotserve/… instead", path, target, ex)
					continue
				}
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s links to %s, which does not exist", path, target)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
