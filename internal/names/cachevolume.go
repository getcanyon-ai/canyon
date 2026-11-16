package names

import "strings"

// packReserved is pack v0.40.3's FilterReservedNames (internal/paths/paths.go), each pair in turn, as it applies them to
// any volume name it is passed, named or not. A replacement can complete the next key ("nulpt": n_u_l_p_t), which one
// pass over the name (strings.Replacer) would miss; in turn, every order gives the same name, so pack's map order is
// immaterial. TestPackIsTheVersionItsReservedNamesWereReadFrom fails when the Makefile's pin moves.
var packReserved = [][2]string{{"aux", "a_u_x"}, {"com", "c_o_m"}, {"con", "c_o_n"}, {"lpt", "l_p_t"}, {"nul", "n_u_l"}, {"prn", "p_r_n"}}

// CacheVolume is the name of an app's build cache volume of kind build or launch, as the executor passes it to pack
// and as pack creates it (contracts/executor.md, /forget).
func CacheVolume(app, kind string) string {
	name := "canyon-cache-" + app + "-" + kind
	for _, r := range packReserved {
		name = strings.ReplaceAll(name, r[0], r[1])
	}
	return name
}
