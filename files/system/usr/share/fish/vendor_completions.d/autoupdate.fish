set -l subcommands on off status help
set -l weekdays mon tue wed thu fri sat sun

complete -c autoupdate -f
complete -c autoupdate -n "not __fish_seen_subcommand_from $subcommands" -a on -d "Turn on / change the schedule"
complete -c autoupdate -n "not __fish_seen_subcommand_from $subcommands" -a off -d "Turn off automatic updates"
complete -c autoupdate -n "not __fish_seen_subcommand_from $subcommands" -a status -d "Show schedule and next run"
complete -c autoupdate -n "not __fish_seen_subcommand_from $subcommands" -a help -d "Show usage"

complete -c autoupdate -n "__fish_seen_subcommand_from on; and not __fish_seen_subcommand_from daily weekly monthly" -a daily -d "Every day at <time>"
complete -c autoupdate -n "__fish_seen_subcommand_from on; and not __fish_seen_subcommand_from daily weekly monthly" -a weekly -d "Every week on <weekday> at <time>"
complete -c autoupdate -n "__fish_seen_subcommand_from on; and not __fish_seen_subcommand_from daily weekly monthly" -a monthly -d "Every month on <day> at <time>"

complete -c autoupdate -n "__fish_seen_subcommand_from weekly; and not __fish_seen_subcommand_from $weekdays" -a "$weekdays"
