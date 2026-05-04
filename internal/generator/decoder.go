package generator

type Parsed struct {
	Timestamp int64
	Region    int64
	Worker    int64
	Seq       int64
}

func Decode(id int64) Parsed {
	return Parsed{
		Timestamp: (id >> 22) + epoch,
		Region:    (id >> 17) & 0x1F,
		Worker:    (id >> 12) & 0x1F,
		Seq:       id & 0xFFF,
	}
}