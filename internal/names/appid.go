package names

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// The words a new app's id is made of (contracts/routes.md, POST /projects {name}; #1911). Curated by
// hand: short, plain and friendly, nothing a person would mind seeing in their app's address, and
// never "canyon", "preview" or "prod". A word is lowercase letters alone, so every id the two make
// with a suffix is a project id.
var (
	idAdjectives = strings.Fields(`amber bold brave breezy bright calm cheery clever cosmic cosy crisp
		daring dapper eager fancy fluffy frosty funny gentle glad golden grand happy humble jazzy jolly
		keen kind lively lucky mellow merry mighty misty neat nimble noble peppy plucky polite proud quick
		quiet rapid rosy shiny silver sleek smooth snappy sparkly steady sturdy sunny swift tidy tranquil
		upbeat velvet vivid warm wise witty zesty`)
	idNouns = strings.Fields(`acorn aspen badger beaver birch bison breeze cedar cloud comet coral crane
		daisy dolphin eagle falcon fern finch fox gecko harbor heron ibis island koala lagoon lark lemur
		lotus lynx maple meadow orchid otter owl panda panther pebble pelican pine puffin quail raven reef
		river robin salmon snake sparrow spruce star stream swan tiger toucan trout tulip walrus willow
		wren yak zebra`)
)

const idSuffix = "abcdefghijklmnopqrstuvwxyz0123456789"

// NewApp is <adjective>-<noun>-<three letters or digits>, funny-snake-4k2, each part from crypto/rand,
// whose reader never fails (Go 1.24 and later crash the program instead).
func NewApp() string {
	pick := func(n int) int {
		i, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
		return int(i.Int64())
	}
	b := []byte(idAdjectives[pick(len(idAdjectives))] + "-" + idNouns[pick(len(idNouns))] + "-")
	for range 3 {
		b = append(b, idSuffix[pick(len(idSuffix))])
	}
	return string(b)
}
