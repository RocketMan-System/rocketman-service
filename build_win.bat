@echo off
echo ========================================
echo Build Script
echo ========================================
echo.

REM Activate virtual environment
if exist "venv\Scripts\activate.bat" (
    echo Activating virtual environment...
    call venv\Scripts\activate.bat
) else (
    echo ERROR: Virtual environment not found
    echo Please run setup script first: setup_venv.bat
    exit /b 1
)

echo Generating version file...
python .\utils\version_gen.py

for /f "delims=" %%n in ('python utils/get_project_name.py') do set EXE_NAME=%%n

REM Check if icon file exists (optional)
set ICON_PARAM=
if exist "icon.ico" (
    echo Using icon file: icon.ico
    set ICON_PARAM=--icon=icon.ico
) else (
    echo Warning: icon.ico not found, building without custom icon
)

echo.
echo Cleaning previous build...
if exist "build" rmdir /s /q build
if exist "dist" rmdir /s /q dist
if exist "%EXE_NAME%.spec" del /q %EXE_NAME%.spec

echo.
echo Building executable with PyInstaller...
echo This may take a few minutes...
echo.

pyinstaller --onefile ^
    --windowed ^
    --name=%EXE_NAME% ^
    %ICON_PARAM% ^
    --version-file version.txt ^
    src/windows/main.py

if errorlevel 1 (
    echo.
    echo ERROR: Build failed
    exit /b 1
)

if exist "version.txt" del /q version.txt

echo.
echo ========================================
echo Build completed successfully!
echo ========================================