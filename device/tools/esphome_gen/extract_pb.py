"""Pulls the serialized FileDescriptorProto out of a generated *_pb2.py by
parsing it (ast), never importing it: the AddSerializedFile(b'...') literal."""
import ast, sys
src, dst = sys.argv[1], sys.argv[2]
tree = ast.parse(open(src).read())
found = [n.args[0].value for n in ast.walk(tree)
         if isinstance(n, ast.Call) and getattr(n.func, "attr", "") == "AddSerializedFile"
         and n.args and isinstance(n.args[0], ast.Constant) and isinstance(n.args[0].value, bytes)]
assert len(found) == 1, len(found)
open(dst, "wb").write(found[0])
print(dst, len(found[0]))
