set -l subcommands enable disable status pair unpair port nodes serve help

complete -c dashboard -f
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a enable -d "Turn on (main node, or: enable node)"
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a disable -d "Turn off"
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a status -d "Mode, address, pairing, fingerprint"
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a pair -d "Node: print a new pairing password"
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a unpair -d "Node: forget the main node"
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a port -d "Change the port"
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a nodes -d "Main: list paired nodes"
complete -c dashboard -n "not __fish_seen_subcommand_from $subcommands" -a help -d "Show usage"

complete -c dashboard -n "__fish_seen_subcommand_from enable; and __fish_is_nth_token 2" -a main -d "The web UI (default)"
complete -c dashboard -n "__fish_seen_subcommand_from enable; and __fish_is_nth_token 2" -a node -d "Managed from a main node"
