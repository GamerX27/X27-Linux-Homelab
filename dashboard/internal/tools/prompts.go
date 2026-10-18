package tools

import "fmt"

// Prompt is a ready-made task that MCP clients list as a prompt.
type Prompt struct {
	Name        string
	Title       string
	Description string
	Args        []string // required arguments
	Text        func(args map[string]string) string
}

var Prompts = []Prompt{
	{Name: "health_check", Title: "Health check all nodes",
		Description: "Look over every node and report what needs attention.",
		Text: func(map[string]string) string {
			return "Do a health check of my homelab. Use list_nodes, then for each online node node_overview, list_containers and " +
				"journal_logs with priority err since 24h. Report, per node: offline nodes, disks over 85% full, memory over 90%, high " +
				"temperatures, containers that are restarting, exited or unhealthy, pending OS and container updates, and recurring errors " +
				"in the journal. End with a short prioritized list of what I should do. Don't change anything."
		}},
	{Name: "troubleshoot_app", Title: "Troubleshoot an app",
		Description: "Find out why an app (compose stack) misbehaves.", Args: []string{"node", "stack"},
		Text: func(a map[string]string) string {
			return fmt.Sprintf("The app %q on node %q isn't working right. Use list_stacks, list_containers, container_logs for each of its "+
				"containers and get_stack to find the cause. Explain what's wrong and what would fix it. Ask me before you change anything.",
				a["stack"], a["node"])
		}},
	{Name: "plan_updates", Title: "Plan updates",
		Description: "What would updating the OS and containers everywhere change?",
		Text: func(map[string]string) string {
			return "Check what's out of date in my homelab: list_nodes, then os_status and container_updates for each online node. Tell me " +
				"which nodes have an OS update (and which update on a schedule anyway), which apps have newer images, and a safe order to " +
				"apply them. Don't start any update until I say so."
		}},
}
