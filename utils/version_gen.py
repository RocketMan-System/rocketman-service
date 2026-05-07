import tomllib
from pathlib import Path

PYPROJECT_FILE = Path("pyproject.toml")
VERSION_FILE = Path("version.txt")

# Читаем pyproject.toml
with PYPROJECT_FILE.open("rb") as f:
    pyproject = tomllib.load(f)

# Берём данные
project = pyproject.get("project", {})
version_str = project.get("version", "0.0.1")
name = project.get("name", "MyApp")
description = project.get("description", "MyApp description")
authors = project.get("authors", [{"name": "MyCompany"}])
company = authors[0].get("name", "MyCompany")

# Разбиваем версию на 4 числа (major, minor, patch, build)
parts = version_str.split(".")
while len(parts) < 4:
    parts.append("0")
filevers = tuple(int(p) for p in parts[:4])
prodvers = filevers

# Генерируем содержимое version.txt
version_txt = f"""
# UTF-8
VSVersionInfo(
  ffi=FixedFileInfo(
    filevers={filevers},
    prodvers={prodvers},
    mask=0x3f,
    flags=0x0,
    OS=0x4,
    fileType=0x1,
    subtype=0x0,
    date=(0, 0)
    ),
  kids=[
    StringFileInfo(
      [
      StringTable(
        '040904B0',
        [StringStruct('CompanyName', '{company}'),
         StringStruct('FileDescription', '{description}'),
         StringStruct('FileVersion', '{version_str}'),
         StringStruct('InternalName', '{name}.exe'),
         StringStruct('LegalCopyright', '© 2026 {company}'),
         StringStruct('OriginalFilename', '{name}.exe'),
         StringStruct('ProductName', '{name}'),
         StringStruct('ProductVersion', '{version_str}')])
      ]),
    VarFileInfo([VarStruct('Translation', [1033, 1200])])
  ]
)
"""

# Сохраняем в файл
VERSION_FILE.write_text(version_txt.strip(), encoding="utf-8")
print(f"version.txt сгенерирован из pyproject.toml: {VERSION_FILE}")
