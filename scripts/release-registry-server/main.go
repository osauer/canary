// Command release-registry-server derives a versioned MCP Registry document
// from server.json and the release bundle's SHA-256 digest.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var releaseVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$`)

const maxRegistryDescriptionLen = 100

type registryServer struct {
	Schema      string             `json:"$schema,omitempty"`
	Name        string             `json:"name"`
	Title       string             `json:"title,omitempty"`
	Description string             `json:"description"`
	Version     string             `json:"version"`
	WebsiteURL  string             `json:"websiteUrl,omitempty"`
	Repository  registryRepository `json:"repository"`
	Packages    []registryPackage  `json:"packages"`
}

type registryRepository struct {
	URL    string `json:"url"`
	Source string `json:"source"`
	ID     string `json:"id,omitempty"`
}

type registryPackage struct {
	RegistryType string            `json:"registryType"`
	Identifier   string            `json:"identifier"`
	FileSHA256   string            `json:"fileSha256"`
	Transport    registryTransport `json:"transport"`
}

type registryTransport struct {
	Type string `json:"type"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "release-registry-server: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: go run ./scripts/release-registry-server vX.Y.Z dist/canary-vX.Y.Z.mcpb dist/server.json")
	}

	releaseVersion, mcpbPath, outputPath := args[0], args[1], args[2]
	if !releaseVersionPattern.MatchString(releaseVersion) {
		return fmt.Errorf("RELEASE_VERSION must look like vX.Y.Z (got %q)", releaseVersion)
	}
	version := strings.TrimPrefix(releaseVersion, "v")

	data, err := os.ReadFile("server.json")
	if err != nil {
		return err
	}

	var server registryServer
	if err := json.Unmarshal(data, &server); err != nil {
		return fmt.Errorf("read server.json: %w", err)
	}
	if server.Name == "" || server.Description == "" {
		return fmt.Errorf("server.json must define name and description")
	}
	if len(server.Description) > maxRegistryDescriptionLen {
		return fmt.Errorf("server.json description must be <= %d characters for MCP Registry (got %d)", maxRegistryDescriptionLen, len(server.Description))
	}
	server.Version = version

	digest, err := fileSHA256(mcpbPath)
	if err != nil {
		return err
	}

	server.Packages = []registryPackage{
		{
			RegistryType: "mcpb",
			Identifier:   fmt.Sprintf("https://github.com/osauer/canary/releases/download/%s/canary-%s.mcpb", releaseVersion, releaseVersion),
			FileSHA256:   digest,
			Transport: registryTransport{
				Type: "stdio",
			},
		},
	}

	out, err := json.MarshalIndent(server, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outputPath, out, 0o644); err != nil {
		return err
	}

	fmt.Printf("release-registry-server: wrote %s for %s (%s)\n", outputPath, releaseVersion, digest)
	return nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
