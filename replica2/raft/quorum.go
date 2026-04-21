package raft

func quorumSize(totalNodes int) int {
	if totalNodes <= 1 {
		return 1
	}
	return totalNodes/2 + 1
}
