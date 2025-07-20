# FSR Server

**This replaces the Python based FSR server as found at [teejusb's FSR repository](https://github.com/teejusb/fsr).** 

This can be run directly as an exe, and does not use Python.

**This is in a usable state, but has some known bugs.** For example, the major issue currently is that it only saves one threshold at a time. Also occasionally you need to start the exe over again if the values aren't updating in the web UI, and it's possible to crash the server by closing the web UI window. Please note issues in the Issues section if you run into any unexpected/unwanted behavior. When I get the threshold saving issue taken care of, I'll start distributing pre-built exe's.

Please use teejusb's page if you need the firmware, since the firmware isn't kept here.

# Building

For now you need to build it yourself. Once I some of the existing bugs worked out I will provide one that is pre-built.

You only need to do this once. After you do it then you get a `server.exe` which you can run directly.

If you already have a working Web UI with Python, then you can skip steps 2 thru 5 and go straight to building the exe.

1.  Install [Go](https://go.dev/doc/install) latest version.
2.  Install [Node.js](https://nodejs.org/en/download) latest version
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

You just pass the needed settings as flags to the program. At a minimum, you have to define the serial port the pad is on.

For example, if your pad is on COM4:

`server.exe --gamepad COM4`

You can also define the web port you want the UI to be accessible from, and the number of sensors.

For example, if your pad is on COM9, and you have 8 sensors in the pad:

`server.exe --gamepad COM9 --sensors 8`

Or if you want to run the server on port 5001 instead of port 5000 (the default):

`server.exe --port 5001`

You  can use `--gamepad`, `--sensors`, and `--port` in any combination.

# Making a shortcut

If you use something like `start_fsrs.bat` to start your server, you can instead maintain this functionality by making a shortcut to the `server.exe`. Then right-click the shortcut and choose "Properties". At the end of the "Target" field, add the flags you need.

<img width="390" height="566" alt="image" src="https://github.com/user-attachments/assets/0c914ac9-6735-4c81-9d8f-b03ad17d3ace" />

# Credits / License

This project is GPLv3 licensed. With the exception of the contents of the `server` directory, everything in the `webui` directory is exactly as it's found in [teejusb's FSR repository](https://github.com/teejusb/fsr) which is GPLv3.
