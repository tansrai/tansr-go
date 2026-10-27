package terminalpersistence

import "github.com/tansrai/tansr-go/canonical"

// authenticationTree uses the shared tansr-authenticated-patricia-sha256-v1
// encoding. A missing node is corruption, never an absent business key. Nodes
// are immutable; only the authenticated root is published by the media owner.
type authenticationTree struct {
	read  func(string) []byte
	write func(string, []byte)
}

type authNode struct {
	key, value, left, right string
	bit                     int
}
type authStep struct {
	node  authNode
	right bool
}

func digestValid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func authHex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}
func authDifference(a, b string) int {
	for i := 0; i < 64; i++ {
		d := authHex(a[i]) ^ authHex(b[i])
		if d != 0 {
			for bit := 0; bit < 4; bit++ {
				if d&(8>>bit) != 0 {
					return i*4 + bit
				}
			}
		}
	}
	return 256
}
func authRight(key string, bit int) bool { return authHex(key[bit/4])&(8>>(bit%4)) != 0 }
func (t authenticationTree) node(digest string, parent *authNode, right bool, path map[string]bool) authNode {
	need(digestValid(digest) && len(path) < 257 && !path[digest])
	path[digest] = true
	b := t.read(digest)
	need(len(b) > 0 && len(b) <= 512 && hash(b) == digest)
	v, err := canonical.ParseStrict(b, canonical.Options{MaxBytes: 512})
	need(err == nil)
	o := m(v)
	result := authNode{key: str(o["key"]), bit: -1}
	need(digestValid(result.key))
	if o["kind"] == "leaf" {
		need(exact(o, "kind", "key", "value"))
		result.value = str(o["value"])
		need(digestValid(result.value))
	} else {
		need(o["kind"] == "branch" && exact(o, "kind", "key", "bit", "left", "right"))
		result.bit = n(o["bit"])
		result.left = str(o["left"])
		result.right = str(o["right"])
		need(result.bit >= 0 && result.bit <= 255 && digestValid(result.left) && digestValid(result.right) && result.left != result.right)
	}
	if parent != nil {
		need((result.value != "" || result.bit > parent.bit) && authDifference(result.key, parent.key) >= parent.bit && authRight(result.key, parent.bit) == right)
	}
	return result
}
func (t authenticationTree) emit(v authNode) string {
	o := obj{"kind": "leaf", "key": v.key, "value": v.value}
	if v.value == "" {
		o = obj{"kind": "branch", "key": v.key, "bit": v.bit, "left": v.left, "right": v.right}
	}
	b := enc(o)
	digest := hash(b)
	t.write(digest, b)
	return digest
}
func (t authenticationTree) get(root, key string) string {
	need(digestValid(key))
	if root == "" {
		return ""
	}
	path := map[string]bool{}
	var parent *authNode
	right := false
	for {
		node := t.node(root, parent, right, path)
		if node.value != "" {
			if node.key == key {
				return node.value
			}
			return ""
		}
		if authDifference(node.key, key) < node.bit {
			return ""
		}
		right = authRight(key, node.bit)
		parent = &node
		if right {
			root = node.right
		} else {
			root = node.left
		}
	}
}
func (t authenticationTree) put(root, key, value string) string {
	need(digestValid(key) && digestValid(value))
	replacement := authNode{key: key, value: value, bit: -1}
	if root == "" {
		return t.emit(replacement)
	}
	path := map[string]bool{}
	steps := []authStep{}
	var parent *authNode
	right := false
	current := root
	result := ""
	for {
		node := t.node(current, parent, right, path)
		different := authDifference(node.key, key)
		if node.value != "" && different == 256 {
			if node.value == value {
				return root
			}
			result = t.emit(replacement)
			break
		}
		if node.value == "" && different >= node.bit {
			right = authRight(key, node.bit)
			steps = append(steps, authStep{node, right})
			parent = &node
			if right {
				current = node.right
			} else {
				current = node.left
			}
			continue
		}
		leaf := t.emit(replacement)
		replacement = authNode{key: key, bit: different, left: leaf, right: current}
		if authRight(key, different) {
			replacement.key = node.key
			replacement.left = current
			replacement.right = leaf
		}
		result = t.emit(replacement)
		break
	}
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		replacement = authNode{key: replacement.key, bit: step.node.bit, left: result, right: step.node.right}
		if step.right {
			replacement.key = step.node.key
			replacement.left = step.node.left
			replacement.right = result
		}
		result = t.emit(replacement)
	}
	return result
}

// visit is only for explicitly requested complete audit/copy/custody work.
func (t authenticationTree) visit(root string, work func(string, string)) {
	if root == "" {
		return
	}
	path := map[string]bool{}
	var visit func(string, *authNode, bool)
	visit = func(h string, p *authNode, right bool) {
		node := t.node(h, p, right, path)
		defer delete(path, h)
		if node.value != "" {
			work(node.key, node.value)
			return
		}
		visit(node.left, &node, false)
		visit(node.right, &node, true)
	}
	visit(root, nil, false)
}
