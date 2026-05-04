package store
import "fmt"
func customerKey(i int) string { return fmt.Sprintf("customer:%d", i) }
