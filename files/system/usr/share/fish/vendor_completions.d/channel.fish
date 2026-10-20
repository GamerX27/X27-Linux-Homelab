set -l channels stable testing testing-alma

complete -c channel -f
complete -c channel -n "not __fish_seen_subcommand_from $channels status help" -a status -d "Show the current channel"
complete -c channel -n "not __fish_seen_subcommand_from $channels status help" -a help -d "Show usage"
complete -c channel -n "not __fish_seen_subcommand_from $channels status help" -l json -d "Show the current channel as JSON"
complete -c channel -n "not __fish_seen_subcommand_from $channels status help" -a stable -d "Fedora 44, production"
complete -c channel -n "not __fish_seen_subcommand_from $channels status help" -a testing -d "Fedora 45, may break"
complete -c channel -n "not __fish_seen_subcommand_from $channels status help" -a testing-alma -d "AlmaLinux 10, may break"
complete -c channel -n "__fish_seen_subcommand_from $channels" -a --reboot -d "Reboot right away"
