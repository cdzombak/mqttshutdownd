package main

import "log"

func StrictLogger(strict bool) func(m string) {
	if strict {
		return func(m string) {
			log.Fatal(m)
		}
	} else {
		return func(m string) {
			log.Println(m)
		}
	}
}

func DebugLogger(debug bool) func(m string) {
	if debug {
		return func(m string) {
			log.Printf("[DEBUG] %s", m)
		}
	} else {
		return func(m string) {}
	}
}
