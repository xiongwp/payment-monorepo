package outbox

import "encoding/json"

func jsonDecode(b []byte, v *Event) error {
	return json.Unmarshal(b, v)
}
