package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed schemas/events.v1.json
var schemaFS embed.FS

const schemaPath = "schemas/events.v1.json"

type Validator struct {
	schema *jsonschema.Schema
}

func NewValidator() (*Validator, error) {
	raw, err := fs.ReadFile(schemaFS, schemaPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource(schemaPath, bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	s, err := c.Compile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return &Validator{schema: s}, nil
}

func (v *Validator) Validate(raw json.RawMessage) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode event: %w", err)
	}
	if err := v.schema.Validate(doc); err != nil {
		return err
	}
	return nil
}
