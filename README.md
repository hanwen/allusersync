This repository shows how to create a Gerrit All-Users.git repository using REST API data.

Usage:

```
$ go run main.go --repo ~/vc/gerrit_testsite/git/All-Users.git/ --cookie git-hanwen.google.com=1//SECRET --url https://gerrit-review.googlesource.com 1024147 1060017 1082084 1084483

$ curl -u admin:"XqDG4yB3JMAIVnrp7BJDC3Q3luc2GIk+UBYUqHH2GQ"  http://localhost:8080/a/accounts/1024147
)]}'
{"_account_id":1024147,"name":"Han-Wen Nienhuys","email":"hanwen@google.com"}
```
