package server

import "math/rand/v2"

// randomName returns a memorable subdomain like "brave-otter".
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

	return adjectives[rand.IntN(len(adjectives))] + "-" + animals[rand.IntN(len(animals))]
}
