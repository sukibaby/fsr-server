# Go-based FSR Server (unfinished)

For now you need to build it yourself. It is in a mostly usable state, but is unfinished, so some thing (like the Profile menu) just don't work yet. After you build it then you get a `server.exe` which you can run directly.

_If you already have a working Web UI setup, then you can skip steps 2 and 3._

1.  Install [Go](https://go.dev/doc/install) latest version.
2.  Install [Node.js](https://nodejs.org/en/download) whatever version is recommended should be fine
3.  Install [Yarn](https://classic.yarnpkg.com/en/docs/install#windows-stable)  by running `npm install -g yarn` after installing Node.js
4.  Open a console (for example, Windows Terminal) and navigate to the `fsr-server` folder by typing `cd ` and then dragging the `fsr-server` folder onto your Terminal.
5.  Run `go mod init`
6.  Enter the `webui` folder with `cd webui`.  
7.  Run `yarn install && yarn build`
8.  Enter the `server` folder with `cd server`.
9.  Build the exe
   -  Windows: `go build -o server.exe .\server.go`
   -  Linux or Mac: `go build -o server ./server.go`

Now the server is built. From now on you just need to run `server.exe` (or `server` if using Linux or Mac) any time you want to open the web UI.

# Usage

Define the pad's serial port with --gamepad

`server.exe --gamepad COM4`

Defining the number of sensors:

`server.exe --sensors 8`

Defining what port the server should use (defaults to 5000):

`server.exe --port 5001`

# Making a shortcut

If you use something like `start_fsrs.bat` to start your server, you can instead maintain this functionality by making a shortcut to the `server.exe`. Then right-click the shortcut and choose "Properties". At the end of the "Target" field, add the flags you need.

<img width="390" height="566" alt="image" src="https://github.com/user-attachments/assets/0c914ac9-6735-4c81-9d8f-b03ad17d3ace" />
