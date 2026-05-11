package mw

import "os"

func _osGetenv(k string) string { return os.Getenv(k) }
