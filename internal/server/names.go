package server

import (
	cryptorand "crypto/rand"
	"math/rand/v2"
	"strings"
)

// suffixLen adds 40 random bits so live tunnels cannot be found by enumerating word pairs.
const suffixLen = 8

// randomName returns a memorable, unguessable subdomain like "brave-otter-k3j9x2mq".
func randomName() string {
	adjectives := [...]string{
		"brave", "calm", "clever", "cosmic", "crisp", "eager", "fancy", "fuzzy",
		"gentle", "happy", "jolly", "keen", "lively", "lucky", "mellow", "mighty",
		"nimble", "plucky", "proud", "quick", "quiet", "rapid", "shiny", "snappy",
		"sunny", "swift", "tidy", "witty", "zesty", "bold", "bright", "cheery",
	}

	animals := [...]string{
		"otter", "falcon", "panda", "koala", "lynx", "badger", "heron", "gecko",
		"walrus", "beaver", "bison", "crane", "dingo", "ferret", "ibis", "jackal",
		"lemur", "marten", "newt", "ocelot", "puffin", "quokka", "raven", "seal",
		"tapir", "toucan", "viper", "wombat", "yak", "zebra", "parrot", "moose",
	}

	suffix := strings.ToLower(cryptorand.Text()[:suffixLen])

	return adjectives[rand.IntN(len(adjectives))] + "-" + animals[rand.IntN(len(animals))] + "-" + suffix
}
