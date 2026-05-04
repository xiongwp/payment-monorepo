package security

const secret int64 = 0x5A5A5A5A5A5A5A5A

func Encode(id int64) int64 { return id ^ secret }
func Decode(id int64) int64 { return id ^ secret }
