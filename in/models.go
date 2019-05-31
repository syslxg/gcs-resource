package in

import (
	gcsresource "github.com/syslxg/gcs-resource"
)

type InRequest struct {
	Source  gcsresource.Source  `json:"source"`
	Version gcsresource.Version `json:"version"`
	Params  Params              `json:"params"`
}

type Params struct {
	SkipDownload bool `json:"skip_download"`
	Unpack       bool `json:"unpack"`
}

type InResponse struct {
	Version  gcsresource.Version        `json:"version"`
	Metadata []gcsresource.MetadataPair `json:"metadata"`
}
