//go:build !unix

package onboarding

func userWritable(string) bool { return false }
