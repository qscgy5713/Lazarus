#!/usr/bin/env bash
# Bash completion script for Lazarus backup restoration verifier
# Usage: source lazarus.bash

_lazarus_completions() {
    local cur prev opts
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"

    opts="--config --target --json --quiet --keep-on-failure --check-config --version"

    case "${prev}" in
        --config)
            COMPREPLY=( $(compgen -f -X '!*.yml' -- "${cur}") $(compgen -f -X '!*.yaml' -- "${cur}") )
            return 0
            ;;
        --target)
            # If config file is present in current directory, suggest targets
            local cfg="lazarus.yml"
            for ((i=1; i<COMP_CWORD; i++)); do
                if [[ "${COMP_WORDS[i]}" == "--config" && -n "${COMP_WORDS[i+1]}" ]]; then
                    cfg="${COMP_WORDS[i+1]}"
                    break
                fi
            done
            if [[ -f "${cfg}" ]]; then
                local targets=$(grep -E '^\s*-\s*name:' "${cfg}" | awk -F'name:' '{print $2}' | tr -d ' "')
                COMPREPLY=( $(compgen -W "${targets}" -- "${cur}") )
                return 0
            fi
            ;;
    esac

    if [[ "${cur}" == -* ]]; then
        COMPREPLY=( $(compgen -W "${opts}" -- "${cur}") )
        return 0
    fi
}

complete -F _lazarus_completions lazarus
