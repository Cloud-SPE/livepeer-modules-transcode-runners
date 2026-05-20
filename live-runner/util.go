package main

import "strconv"

func itoa(v int) string { return strconv.Itoa(v) }

func itoa64(v uint64) string { return strconv.FormatUint(v, 10) }
