package credential

import "os"

func osStat(p string) (os.FileInfo, error) { return os.Stat(p) }
func writeFile(p string) error             { return os.WriteFile(p, []byte("x"), 0o600) }
