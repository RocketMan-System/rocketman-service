import tomllib
with open("pyproject.toml", "rb") as f:
    data = tomllib.load(f)

# PEP 621
project_name = data.get("project", {}).get("name", "WinPowerControl")
print(project_name)
