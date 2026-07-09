/* All player-facing narrative text. Editing the story = editing this file. */
export const STORY = {
  intro: [
    "You wake up in your room in Willow Town.",
    "Today you receive your first Shapemon!",
    "Visit Prof. Cedar's lab, then head north to",
    "challenge the Fernwood Gym. The Heal Center",
    "and Rocky Cave are open to explore, too.",
  ],
  profGive: [
    "Prof. Cedar: There you are!",
    "As arranged, your partner is the Fire-type,",
    "Emberling. Take good care of it!",
    "Emberling joined your party!",
    "Now earn the Leaf Badge at Fernwood Gym, north of town.",
  ],
  profDone: ["Prof. Cedar: Emberling's flame burns bright. Go get that badge!"],
  nurse: ["Nurse: Welcome to the Heal Center!", "Your Shapemon are fully healed."],
  gymNoStarter: ["Leader Fern: No Shapemon? Come back when you're ready."],
  gymIntro: [
    "Leader Fern: Welcome to Fernwood Gym!",
    "My Grass-types have deep roots. Show me your fire!",
  ],
  gymDone: [
    "Leader Fern: The Leaf Badge suits you. Well fought!",
    "The gate north of town is open now. Beyond it lies",
    "Tidewater Town and its Water gym. Good luck!",
  ],
  gym2NoStarter: ["Leader Marina: You wandered in without a team? Off you go."],
  gym2Intro: [
    "Leader Marina: So you cleared Fernwood. Impressive.",
    "But my Water-types will douse that flame. Come on!",
  ],
  gym2Done: [
    "Leader Marina: Two badges! You're the real deal.",
    "The gate north opens to Cinder Village and its gym.",
  ],
  gym3NoStarter: ["Leader Rocco: A challenger with no team? Go train first."],
  gym3Intro: [
    "Leader Rocco: Cinder Village's gym is the last test.",
    "My Normal-types hit hard and never quit. Bring it!",
  ],
  gym3Done: [
    "Leader Rocco: Three badges! You've mastered the basics.",
    "The Victory Gate beyond the village is finally open.",
  ],
  gateLocked0: [
    "A gate blocks the way north.",
    "You need the Leaf Badge to pass. Beat Fernwood Gym first!",
  ],
  gateLocked1: [
    "This gate leads to Cinder Village.",
    "Earn the Tidewater Badge from Leader Marina first!",
  ],
  gateLocked2: [
    "This gate leads on to Voltage City.",
    "Earn the Cinder Badge from Leader Rocco first!",
  ],
  gym4Intro: ["Leader Volt: My Electric-types are lightning fast.", "Try to keep up!"],
  gym4Done: ["Leader Volt: Shocking skill! Stonehaven's gym is next."],
  gym5Intro: ["Leader Terra: My Rock-types are an immovable wall.", "Break through if you can!"],
  gym5Done: ["Leader Terra: Solid work. Glacia Town awaits you."],
  gym6Intro: ["Leader Frost: My Ice-types will freeze you solid.", "Bring the heat!"],
  gym6Done: ["Leader Frost: Icy-cool battling. On to Mindspire!"],
  gym7Intro: ["Leader Sage: My Psychic-types see your every move.", "Prove me wrong!"],
  gym7Done: ["Leader Sage: The mind is strong in you. Miasma Marsh is last."],
  gym8Intro: ["Leader Venia: My Poison-types are the final trial.", "This is where your journey is decided!"],
  gym8Done: ["Leader Venia: Eight badges! You are a true Champion!"],
  gateLocked3: ["This gate leads to Stonehaven.", "Earn the Voltage Badge from Leader Volt first!"],
  gateLocked4: ["This gate leads to Glacia Town.", "Earn the Stone Badge from Leader Terra first!"],
  gateLocked5: ["This gate leads to Mindspire.", "Earn the Glacier Badge from Leader Frost first!"],
  gateLocked6: ["This gate leads to Miasma Marsh.", "Earn the Mind Badge from Leader Sage first!"],
  gateLocked7: ["The final Victory Gate is sealed.", "Earn the Miasma Badge from Leader Venia first!"],

  // Walk-up NPC chatter, keyed by the npc's `dialog` id (see data/maps.js).
  npc: {
    kid: [
      "Kid: The tall grass is full of wild Shapemon!",
      "Walk through it and you might run into one.",
    ],
    oldman: [
      "Old Man: Fire beats Grass, Grass beats Water,",
      "and Water beats Fire. Type match-ups win battles!",
    ],
    hiker: [
      "Hiker: Rocky Cave to the north-east is crawling",
      "with tough Shapemon. Heal up before you go in!",
    ],
    swimmer: [
      "Swimmer: Catch a Grass-type in the tall grass here —",
      "it'll make short work of Marina's Water gym!",
    ],
    elder: [
      "Elder: Cinder Village is the end of the road.",
      "Rocco's Normal-types have no weakness — out-level them!",
    ],
  },

  // Trainer battles, keyed by the npc's `dialog` id (see data/maps.js).
  trainers: {
    rick: {
      intro: ["Camper Rick: Hey, you look tough!", "Let's battle!"],
      win: ["Camper Rick: Wow, your team is strong!"],
    },
    mia: {
      intro: ["Scout Mia: A trainer never backs down.", "Here I come!"],
      win: ["Scout Mia: You've really bonded with your Shapemon."],
    },
    kai: {
      intro: ["Sailor Kai: These waters are my turf!", "Prove yourself!"],
      win: ["Sailor Kai: You've got the current on your side."],
    },
    bruno: {
      intro: ["Ranger Bruno: You made it to Cinder Village.", "Let's see if you're ready for Rocco!"],
      win: ["Ranger Bruno: Rocco won't go as easy as I did."],
    },
    ace: {
      intro: ["Ace Trainer: Only the best make it this far.", "Show me what you've got!"],
      win: ["Ace Trainer: You're gym material, no doubt about it."],
    },
  },

  shopWelcome: ["Clerk: Welcome to the Mart! What can I get you?"],
  pcBoot: ["You booted up the Storage PC."],
};

// Rolling credits shown after clearing the final gym.
export const CREDITS = [
  "THANK YOU FOR PLAYING!", "", "You collected all 8 Badges and became",
  "the Shapemon Champion of the region!", "", "Shapemon — Ember Quest",
  "An original Gen-1/3-style demo", "",
  "Badges .. Leaf, Tidewater, Cinder, Voltage,",
  "          Stone, Glacier, Mind, Miasma", "",
  "Battle engine .. authentic FireRed math",
  "Design & Code .. you + Claude", "", "~ The End ~",
];
