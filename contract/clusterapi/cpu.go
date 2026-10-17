package clusterapi

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// cpuQuantity reads a job's cpu, which may be written as a number (cpu: 4) or as a string ("4",
// "500m"). It is kept and answered as the string.
type cpuQuantity string

func (q *cpuQuantity) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, (*string)(q))
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return fmt.Errorf(`cpu must be a number or a string such as "4" or "500m"`)
	}
	*q = cpuQuantity(number.String())
	return nil
}

func (j *JobSpec) UnmarshalJSON(data []byte) error {
	type plain JobSpec
	in := struct {
		*plain
		CPU cpuQuantity `json:"cpu"`
	}{plain: (*plain)(j)}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	j.CPU = string(in.CPU)
	return nil
}

func (g *JobGroup) UnmarshalJSON(data []byte) error {
	type plain JobGroup
	in := struct {
		*plain
		CPU cpuQuantity `json:"cpu"`
	}{plain: (*plain)(g)}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	g.CPU = string(in.CPU)
	return nil
}
