package domain

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
)

// cpuQuantity reads a job's cpu from a request, which may write it as a number (cpu: 4) or as a
// string ("4", "500m"). It is kept and answered as the string.
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

// cpuEitherForm documents and checks cpu as either form in the schema of a struct that holds it.
func cpuEitherForm(s *huma.Schema) *huma.Schema {
	if s.Properties["cpu"] != nil {
		s.Properties["cpu"] = &huma.Schema{
			Description: `CPU for each node: a number of cores (4) or a quantity ("500m"). Always stated. Answered as a string.`,
			OneOf:       []*huma.Schema{{Type: huma.TypeString}, {Type: huma.TypeNumber, Minimum: new(float64)}},
		}
	}
	return s
}

func (JobSpec) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema { return cpuEitherForm(s) }
func (JobGroup) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return cpuEitherForm(s)
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
