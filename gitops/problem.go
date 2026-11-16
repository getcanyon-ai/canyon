// Package gitops is the deploy repo under delivery: repo (design/HAND-OFF.md §3b; contracts/routes.md, The
// deploy repo): what Canyon renders for a release, from typed objects and the company's templates, and how it
// commits that to the company's repo and opens a pull request — every failure a problem code with its D5
// sentence, never the remote's words, and the repo's token never in either.
package gitops

// The problem codes (contracts/routes.md, The deploy repo; release.schema.json's repoProblem).
const (
	Unreachable    = "unreachable"
	TLSUntrusted   = "tls_untrusted"
	AuthRefused    = "auth_refused"
	BranchMissing  = "branch_missing"
	NoWrite        = "no_write"
	Protected      = "protected_branch"
	PullsAPI       = "pulls_api_unavailable"
	TemplateMiss   = "template_missing"
	TemplateBad    = "template_invalid"
	DirectoryTaken = "directory_taken"
	NameTooLong    = "name_too_long"
)

// Problem is why the deploy repo did not take a release, or a check failed: a code and its sentence.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *Problem) Error() string { return p.Code + ": " + p.Message }

// Retries is whether a release tried again later may land: the network and the host's own failures are; a
// certificate Canyon does not trust, a refusal of the token, a branch, a rule or a template is not — no retry fixes
// those.
func (p *Problem) Retries() bool {
	return p.Code == Unreachable || p.Code == PullsAPI
}

// Unreached is unreachable for a caller: an attempt its own bound stopped (contracts/routes.md, The deploy repo).
func Unreached() *Problem { return unreachable() }

// The D5 sentences (design/EXPERIENCE.md, the deploy repo's problems), each filled with what it names and
// nothing the remote said.
func unreachable() *Problem {
	return &Problem{Unreachable, "Canyon couldn't reach the repo. Canyon's server must be allowed to connect to it: a git host at a private address needs a rule in Canyon's network policy."}
}

func tlsUntrusted() *Problem {
	return &Problem{TLSUntrusted, "Canyon doesn't trust the repo's certificate. Give Canyon's server the CA that signed it: A git host at a private address, under Your deploy repo in Canyon's docs."}
}

func authRefused() *Problem {
	return &Problem{AuthRefused, "The repo refused the token. Check it is current and has this repository, and replace it here."}
}

func branchMissing(branch string) *Problem {
	return &Problem{BranchMissing, "The branch " + branch + " isn't in the repo. Create it, or name another."}
}

func noWrite() *Problem {
	return &Problem{NoWrite, "The token can read the repo but can't push to it. It needs Contents read and write."}
}

func protected(branch string) *Problem {
	return &Problem{Protected, "The repo refused a push to " + branch + ": a branch rule. Production goes through a pull request, and preview needs a branch Canyon may push to."}
}

func pullsAPI(apiURL string) *Problem {
	return &Problem{PullsAPI, "Canyon couldn't use the pull requests API at " + apiURL + ". Check the address, and that the token has Pull requests read and write."}
}

func templateMissing(file string) *Problem {
	return &Problem{TemplateMiss, file + " is missing: Canyon needs it to render this app. Ask your platform team."}
}

func templateInvalid(file, reason string) *Problem {
	return &Problem{TemplateBad, file + ": " + reason + ". Ask your platform team."}
}

func nameTooLong(namespace string) *Problem {
	return &Problem{NameTooLong, namespace + " is longer than 63 characters, the most a namespace takes: a shorter app name fits."}
}

func directoryTaken(path, host string) *Problem {
	return &Problem{DirectoryTaken, path + " was written by another Canyon, " + host + ": Canyon never writes over it."}
}

func directoryForeign(path string) *Problem {
	return &Problem{DirectoryTaken, path + " is already in the repo and Canyon didn't write it: Canyon never writes over it."}
}

// The template reasons (design/EXPERIENCE.md, template_invalid's <reason>), the first a template fails.
const (
	notYAML       = "it doesn't parse as YAML"
	anchored      = "it uses an anchor or an alias"
	mergeKey      = "it uses a merge key"
	repeatedKey   = "it repeats a key"
	foreignTag    = "it uses a tag outside YAML's core schema"
	keyVariable   = "it puts ${…} in a key"
	unknownVar    = "it uses a variable Canyon doesn't have"
	tooBig        = "it is over 64 KiB"
	tooMany       = "it makes more than 10 objects"
	unnamed       = "an object has no apiVersion, kind or metadata.name"
	otherNS       = "it sets another namespace"
	clusterScoped = "it makes a cluster-scoped object"
	collides      = "it makes an object Canyon makes, or one another template makes"
	noAddress     = "it has no canyon.dev/address, or more than one"
	badAddress    = "its canyon.dev/address isn't an https address"
	noEndpoint    = "it has no canyon.dev/endpoint, or more than one"
)

func badEndpoint(typ string) string { return "its canyon.dev/endpoint isn't a " + typ + " address" }

// IsProblem is err's problem when it is one.
func IsProblem(err error) (*Problem, bool) {
	p, ok := err.(*Problem) //nolint:errorlint // every Problem is returned as itself, never wrapped
	return p, ok
}
