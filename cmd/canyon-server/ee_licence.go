//go:build !noee

package main

import "github.com/getcanyon-ai/canyon/ee/licence"

// Canyon Enterprise's licence (ee/licence): the compiled keys and CANYON_LICENSE, read and verified there alone.
func init() {
	eeLicence = func(getenv func(string) string) (licenceHook, error) {
		l, err := licence.FromEnv(getenv)
		if err != nil {
			return nil, err
		}
		return l, nil
	}
}
