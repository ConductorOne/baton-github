package main

import (
	cfg "github.com/conductorone/baton-github/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/config"
)

func main() {
	config.Generate("github", cfg.Config)
}
