// Command release-registry-server derives a versioned MCP Registry document
// from an explicit release-tag server.json template and the release bundle's
// SHA-256 digest.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	if len(args) != 4 {
		return fmt.Errorf("usage: go run ./scripts/release-registry-server vX.Y.Z release-server.json dist/canary-vX.Y.Z.mcpb dist/server.json")
	}

	releaseVersion, templatePath, mcpbPath, outputPath := args[0], args[1], args[2], args[3]
	if !releaseVersionPattern.MatchString(releaseVersion) {
		return fmt.Errorf("RELEASE_VERSION must look like vX.Y.Z (got %q)", releaseVersion)
	}
	version := strings.TrimPrefix(releaseVersion, "v")

	data, err := readRegularFile(templatePath, "release server template")
	if err != nil {
		return err
	}

	var server registryServer
	if err := json.Unmarshal(data, &server); err != nil {
		return fmt.Errorf("read release server template: %w", err)
	}
	if server.Name == "" || server.Description == "" {
		return fmt.Errorf("release server template must define name and description")
	}
	if len(server.Description) > maxRegistryDescriptionLen {
		return fmt.Errorf("release server template description must be <= %d characters for MCP Registry (got %d)", maxRegistryDescriptionLen, len(server.Description))
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
	data, err := readRegularFile(path, "release MCPB")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func readRegularFile(path, label string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", label, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %q must be a regular file", label, path)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", label, path, err)
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", label, path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%s %q changed while opening", label, path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", label, path, err)
	}
	return data, nil
}
