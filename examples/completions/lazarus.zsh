#compdef lazarus
# Zsh completion script for Lazarus backup restoration verifier
# Place in your fpath (e.g. ~/.zsh/completion/_lazarus)

_lazarus() {
    local -a opts
    opts=(
        '--config[Path to configuration YAML file]:config file:_files -g "*.yml *.yaml"'
        '--target[Verify only a specific target]:target name:_lazarus_targets'
        '--json[Machine-readable JSON output format]'
        '--quiet[Only print failing targets and final summary]'
        '--keep-on-failure[Keep container or temp copy upon verification failure for inspection]'
        '--check-config[Validate configuration syntax and defaults without touching Docker]'
        '--version[Print version and exit]'
    )

    _arguments -s $opts
}

_lazarus_targets() {
    local cfg="lazarus.yml"
    local -a targets

    # Check if --config was specified in words
    local idx=${words[(I)--config]}
    if (( idx > 0 && idx < ${#words} )); then
        cfg="${words[idx+1]}"
    fi

    if [[ -f "$cfg" ]]; then
        targets=(${(f)"$(grep -E '^\s*-\s*name:' "$cfg" | awk -F'name:' '{print $2}' | tr -d ' "')"})
        _describe 'target' targets
    fi
}

_lazarus "$@"
