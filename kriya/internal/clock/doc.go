// Package clock provides controllable time.
//
// NOT a declared avspec module. time.Now is forbidden everywhere else by
// lint, because every recovery and fencing test needs to control it and
// retrofitting that later is not practical.
package clock
