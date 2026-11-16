package trivy

import "testing"

// A review finding and an image component name one package when their purl type, name and version agree
// (contracts/finding.md, The built app): the finding's type is its file's by the file's name alone, a Maven
// component's name is its namespace and name as trivy names a Java package, a scoped npm one's its scope and
// name, and a Python name compares as PEP 503 normalises it. A file of no known type names no package.
func TestAReviewFindingAndAnImageComponentNameOnePackage(t *testing.T) {
	for _, c := range []struct {
		file, name, version, purl, componentVersion string
		same                                        bool
	}{
		{"package-lock.json", "express", "4.19.2", "pkg:npm/express@4.19.2", "4.19.2", true},
		{"web/yarn.lock", "@babel/core", "7.24.0", "pkg:npm/%40babel/core@7.24.0", "7.24.0", true},
		{"pom.xml", "org.yaml:snakeyaml", "1.33", "pkg:maven/org.yaml/snakeyaml@1.33", "1.33", true},
		{"lib/snakeyaml-1.33.jar", "org.yaml:snakeyaml", "1.33", "pkg:maven/org.yaml/snakeyaml@1.33", "1.33", true},
		{"requirements.txt", "Zope.Interface", "6.0", "pkg:pypi/zope-interface@6.0", "6.0", true},
		{"go.mod", "github.com/gin-gonic/gin", "v1.9.0", "pkg:golang/github.com/gin-gonic/gin@v1.9.0", "v1.9.0", true},
		{"package-lock.json", "express", "4.19.2", "pkg:npm/express@4.19.2", "4.20.0", false}, // another version
		{"requirements.txt", "express", "4.19.2", "pkg:npm/express@4.19.2", "4.19.2", false},  // another type
		{"Gemfile.lock", "rack", "3.0.0", "pkg:gem/rack@3.0.0", "3.0.0", true},
	} {
		reported, ok := KeyOfFile(c.file, c.name, c.version)
		image, keyed := KeyOf(Component{PURL: c.purl, Version: c.componentVersion})
		if !ok || !keyed || (reported == image) != c.same {
			t.Errorf("%s %s %s against %s: %+v %+v", c.file, c.name, c.version, c.purl, reported, image)
		}
	}
	if _, ok := KeyOfFile("notes.txt", "express", "4.19.2"); ok {
		t.Error("a file of no known type names a package")
	}
	if _, ok := KeyOf(Component{Name: "prebuilt", Version: "1"}); ok {
		t.Error("a component with no purl names a package")
	}
}
