with import <nixpkgs> {};

stdenv.mkDerivation {
name = "python-env";

buildInputs = [
    python313
    python313Packages.pip
    python313Packages.virtualenv
		python313Packages.playwright
];

SOURCE_DATE_EPOCH = 315532800;
PROJDIR = "/tmp/python-env";
S_CONTAINER=false;

shellHook = ''
    echo "Using ${python313.name}"
    export LD_LIBRARY_PATH="${pkgs.stdenv.cc.cc.lib}/lib"

    [ ! -d '$PROJDIR' ] && virtualenv $PROJDIR && echo "SETUP python-dev: DONE"
    source $PROJDIR/bin/activate
    export LC_ALL=en_US.UTF-8

    '';
}

