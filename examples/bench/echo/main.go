/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Command echo runs a quiet TCP echo server for load testing.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/urpc/uio"
)

func main() {
	addr := flag.String("addr", ":9527", "listen address")
	pollers := flag.Int("pollers", 0, "event loop count; zero uses the uio default")
	buffer := flag.Int("buffer", 4096, "maximum bytes read per socket read")
	lockOS := flag.Bool("lockos", false, "lock each event loop to its OS thread")
	reusePort := flag.Bool("reuseport", false, "set SO_REUSEPORT on the listener")
	flag.Parse()

	var events uio.Events
	events.Pollers = *pollers
	events.MaxBufferSize = *buffer
	events.LockOSThread = *lockOS
	events.ReusePort = *reusePort

	events.OnData = func(c uio.Conn) error {
		_, err := c.WriteTo(c)
		return err
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, os.Kill)
		<-sig
		events.Close(nil)
	}()

	if err := events.Serve(*addr); nil != err {
		fmt.Println("server exited with error:", err)
		os.Exit(1)
	}
}
