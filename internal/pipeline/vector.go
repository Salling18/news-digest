package pipeline

import (
	"bytes"
	"encoding/binary"
)

func encodeVector(vec []float32) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, vec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeVector(blob []byte) ([]float32, error) {
	vec := make([]float32, len(blob)/4)
	if err := binary.Read(bytes.NewReader(blob), binary.LittleEndian, &vec); err != nil {
		return nil, err
	}
	return vec, nil
}
