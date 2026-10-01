{
  description = "Autonomous support-triage agent: pinned toolchain for a high-parity local environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "x86_64-linux" "aarch64-linux" ];

      # Terraform is BSL-licensed ("unfree" in nixpkgs); allow only that package.
      pkgsFor = system: import nixpkgs {
        inherit system;
        config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [ "terraform" ];
      };

      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f (pkgsFor system));
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            # Go
            go_1_27
            gopls
            gotools # goimports
            golangci-lint
            delve

            # Database
            goose
            postgresql_18 # psql client (the server runs in Docker)

            # Infrastructure as code
            terraform
            tflint
            trivy

            # Supply chain: known-vulnerable Go dependencies, committed secrets
            govulncheck
            gitleaks

            # CI
            actionlint # lints .github/workflows (uses shellcheck on run: blocks)
            shellcheck

            # GitHub CLI (config kept per-project via GH_CONFIG_DIR, see .envrc)
            gh

            # Cloud + local LLM
            google-cloud-sdk
            ollama

            # Utilities
            # git: the devShell points DEVELOPER_DIR at Nix's Apple SDK, which
            # breaks macOS's /usr/bin/git shim ("error: tool 'git' not found").
            git
            gnumake
            jq
          ];

          # Never let `go` download a different toolchain behind Nix's back.
          GOTOOLCHAIN = "local";
        };
      });
    };
}
